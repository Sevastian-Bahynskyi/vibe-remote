package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/install"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
	systemstate "github.com/Sevastian-Bahynskyi/vibe-remote/internal/system"
)

// The operations below are the dashboard's business layer. They take a context
// and plain arguments, never an http.Request, so the JSON API and the rendered
// HTML dashboard drive identical behaviour instead of two drifting copies.
//
// Each operation that mutates a slot takes s.opMu itself. The lock lives here
// rather than in the handlers so a new caller cannot forget it; no operation
// calls another operation, so there is nothing to deadlock against.

// opError carries the status the two renderers must agree on. Without it the
// HTML layer would have to re-derive "is this a conflict?" from the message text.
type opError struct {
	status int
	err    error
}

func (e *opError) Error() string { return e.err.Error() }
func (e *opError) Unwrap() error { return e.err }

func fail(status int, err error) error {
	return &opError{status: status, err: err}
}

func failText(status int, message string) error {
	return &opError{status: status, err: errors.New(message)}
}

// failStart and failStop keep the status codes the JSON API already returns: a
// Claude turn that would not stop is a conflict the caller may retry with force,
// anything else is a bad request.
func failStart(err error) error { return fail(startFailureStatus(err), err) }

func failStop(err error) error { return fail(stopFailureStatus(err), err) }

// statusOf reports the HTTP status an operation's error should produce.
// An error that never passed through fail is a bug rather than user input, so
// it becomes a 500 and errorMessage redacts it.
func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var opErr *opError
	if errors.As(err, &opErr) {
		return opErr.status
	}
	return http.StatusInternalServerError
}

// forcible reports whether the caller may retry the same operation with force.
// It reads the sentinel rather than the message, so rewording the sentence the
// user sees cannot change the behaviour.
func forcible(err error) bool {
	return errors.Is(err, claude.ErrStopTimeout)
}

// ---- accounts ----

func (s *Server) addAccountOp(email string) error {
	if strings.TrimSpace(email) == "" {
		return failText(http.StatusBadRequest, "a valid email is required")
	}
	if err := launchAccountLogin(s.binary, email); err != nil {
		return fail(http.StatusInternalServerError, err)
	}
	return nil
}

func (s *Server) removeAccountOp(ctx context.Context, id string, deleteCheckpoints bool) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var removeErr error
	if deleteCheckpoints {
		removeErr = s.claude.RemoveProfile(ctx, id, false)
	} else {
		removeErr = s.claude.Remove(ctx, id, false)
	}
	if removeErr != nil {
		return failStop(removeErr)
	}
	if deleteCheckpoints {
		if err := s.store.DeleteAccountAndSessions(ctx, id); err != nil {
			return fail(http.StatusInternalServerError, err)
		}
	}
	return nil
}

// refreshAccountOp re-verifies a sign-in. When verification fails it reopens the
// official login on this Mac; reopened reports which of the two happened so the
// caller can keep the 202-versus-200 distinction.
func (s *Server) refreshAccountOp(ctx context.Context, id string) (account model.Account, reopened bool, err error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	account, refreshErr := s.claude.Refresh(ctx, id)
	if refreshErr != nil {
		if launchErr := launchAccountRelogin(s.binary, id); launchErr != nil {
			return model.Account{}, false, fail(http.StatusInternalServerError, launchErr)
		}
		return model.Account{}, true, nil
	}
	return account, false, nil
}

// ---- workspaces ----

func (s *Server) addWorkspaceOp(ctx context.Context, label, rawPath string) (model.Workspace, error) {
	path, err := validateWorkspace(rawPath)
	if err != nil {
		return model.Workspace{}, fail(http.StatusBadRequest, err)
	}
	workspace, err := s.store.UpsertWorkspace(ctx, model.Workspace{
		Label: strings.TrimSpace(label), Path: path, Selected: true,
	})
	if err != nil {
		return model.Workspace{}, fail(http.StatusBadRequest, err)
	}
	return workspace, nil
}

func (s *Server) removeWorkspaceOp(ctx context.Context, id string) error {
	if err := s.store.DeleteWorkspace(ctx, id); err != nil {
		return fail(http.StatusNotFound, err)
	}
	return nil
}

// ---- slots ----

func (s *Server) createSlotOp(ctx context.Context, body remoteSessionRequest) (model.RemoteSession, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	account, workspace, err := s.resolveRouting(ctx, body.AccountID, body.WorkspaceID)
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusBadRequest, err)
	}
	resume, err := s.resolveResume(ctx, body.ResumeSessionID, "", account.ID, workspace.Path)
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusBadRequest, err)
	}
	existing, err := s.store.ListRemoteSessions(ctx)
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusInternalServerError, err)
	}
	remote, err := s.store.UpsertRemoteSession(ctx, model.RemoteSession{
		Name:            uniqueSessionName(body.Name, account.Email, workspace, existing, ""),
		AccountID:       account.ID,
		WorkspaceID:     workspace.ID,
		WorkspacePath:   workspace.Path,
		ResumeSessionID: resume,
		Desired:         model.DesiredRunning,
	})
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusBadRequest, err)
	}
	if err := s.startRemote(ctx, remote, body.Force); err != nil {
		_ = s.store.DeleteRemoteSession(ctx, remote.ID)
		return model.RemoteSession{}, failStart(err)
	}
	_ = s.store.SelectWorkspace(ctx, workspace.ID)
	return remote, nil
}

// updateSlotOp moves a slot to another account, workspace, or conversation and
// restarts it so the change takes effect.
func (s *Server) updateSlotOp(ctx context.Context, id string, body remoteSessionRequest) (model.RemoteSession, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	remote, err := s.store.GetRemoteSession(ctx, id)
	if err != nil {
		return model.RemoteSession{}, failText(http.StatusNotFound, "remote session not found")
	}
	accountID := remote.AccountID
	if strings.TrimSpace(body.AccountID) != "" {
		accountID = body.AccountID
	}
	workspaceID := remote.WorkspaceID
	if strings.TrimSpace(body.WorkspaceID) != "" {
		workspaceID = body.WorkspaceID
	}
	account, workspace, err := s.resolveRouting(ctx, accountID, workspaceID)
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusBadRequest, err)
	}
	// A conversation belongs to the account and workspace it started in, so
	// moving the slot elsewhere drops the link even when the request asked to
	// keep it.
	kept := remote.ResumeSessionID
	if account.ID != remote.AccountID || workspace.ID != remote.WorkspaceID {
		kept = ""
	}
	resume, err := s.resolveResume(ctx, body.ResumeSessionID, kept, account.ID, workspace.Path)
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusBadRequest, err)
	}
	if err := s.stopRemote(ctx, remote, body.Force); err != nil {
		return model.RemoteSession{}, failStop(err)
	}
	existing, err := s.store.ListRemoteSessions(ctx)
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusInternalServerError, err)
	}
	name := remote.Name
	if strings.TrimSpace(body.Name) != "" || account.ID != remote.AccountID || workspace.ID != remote.WorkspaceID {
		name = uniqueSessionName(body.Name, account.Email, workspace, existing, remote.ID)
	}
	remote.Name = name
	remote.AccountID = account.ID
	remote.WorkspaceID = workspace.ID
	remote.WorkspacePath = workspace.Path
	remote.ResumeSessionID = resume
	remote.Desired = model.DesiredRunning
	remote, err = s.store.UpsertRemoteSession(ctx, remote)
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusBadRequest, err)
	}
	if err := s.startRemote(ctx, remote, true); err != nil {
		return model.RemoteSession{}, failStart(err)
	}
	_ = s.store.SelectWorkspace(ctx, workspace.ID)
	return remote, nil
}

// lifecycleSlotOp starts a slot, or restarts it when restart is set.
func (s *Server) lifecycleSlotOp(ctx context.Context, id string, restart, force bool) (model.RemoteSession, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	remote, err := s.store.GetRemoteSession(ctx, id)
	if err != nil {
		return model.RemoteSession{}, failText(http.StatusNotFound, "remote session not found")
	}
	if restart {
		if err := s.stopRemote(ctx, remote, force); err != nil {
			return model.RemoteSession{}, failStop(err)
		}
	}
	remote, err = s.store.SetRemoteSessionDesired(ctx, remote.ID, model.DesiredRunning)
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusInternalServerError, err)
	}
	if err := s.startRemote(ctx, remote, restart || force); err != nil {
		return model.RemoteSession{}, failStart(err)
	}
	return remote, nil
}

func (s *Server) stopSlotOp(ctx context.Context, id string, force bool) (model.RemoteSession, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	remote, err := s.store.GetRemoteSession(ctx, id)
	if err != nil {
		return model.RemoteSession{}, failText(http.StatusNotFound, "remote session not found")
	}
	if err := s.stopRemote(ctx, remote, force); err != nil {
		return model.RemoteSession{}, failStop(err)
	}
	if _, err := s.store.SetRemoteSessionDesired(ctx, remote.ID, model.DesiredStopped); err != nil {
		return model.RemoteSession{}, fail(http.StatusInternalServerError, err)
	}
	return remote, nil
}

func (s *Server) deleteSlotOp(ctx context.Context, id string) (model.RemoteSession, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	remote, err := s.store.GetRemoteSession(ctx, id)
	if err != nil {
		return model.RemoteSession{}, failText(http.StatusNotFound, "remote session not found")
	}
	if err := s.stopRemote(ctx, remote, true); err != nil {
		return model.RemoteSession{}, failStop(err)
	}
	if err := s.store.DeleteRemoteSession(ctx, remote.ID); err != nil {
		return model.RemoteSession{}, fail(http.StatusNotFound, err)
	}
	return remote, nil
}

// openDesktopOp hands a running slot's Remote Control link to Claude Desktop on
// this Mac. local must come from isLocalRequest: the tailnet is refused because
// the app would open here, not on the phone that asked.
func (s *Server) openDesktopOp(ctx context.Context, id string, local bool) (model.RemoteSession, error) {
	if !local {
		return model.RemoteSession{}, failText(http.StatusForbidden, "Claude Desktop can only be opened from this Mac")
	}
	remote, err := s.store.GetRemoteSession(ctx, id)
	if err != nil {
		return model.RemoteSession{}, failText(http.StatusNotFound, "remote session not found")
	}
	worker := s.claude.Status(remote.ID)
	if !worker.Running || !isClaudeRemoteURL(worker.RemoteURL) {
		return model.RemoteSession{}, failText(http.StatusConflict, "this session has no live Remote Control link yet")
	}
	app := systemstate.ClaudeDesktopApp()
	if app == "" {
		return model.RemoteSession{}, failText(http.StatusPreconditionFailed, "Claude Desktop is not installed on this Mac")
	}
	if deepLink, ok := claudeDesktopDeepLink(worker.RemoteURL); ok {
		if _, err := exec.Command("/usr/bin/open", deepLink).CombinedOutput(); err == nil {
			return remote, nil
		}
	}
	output, err := exec.Command("/usr/bin/open", "-a", app, worker.RemoteURL).CombinedOutput()
	if err != nil {
		return model.RemoteSession{}, fail(http.StatusInternalServerError,
			fmt.Errorf("open Claude Desktop: %w: %s", err, strings.TrimSpace(string(output))))
	}
	return remote, nil
}

// ---- checkpoints and handoffs ----

func (s *Server) updateSessionOp(ctx context.Context, id, title string, pinned bool) (model.Session, error) {
	session, err := s.store.UpdateSessionMetadata(ctx, id, title, pinned)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.Session{}, fail(http.StatusNotFound, err)
		}
		return model.Session{}, fail(http.StatusBadRequest, err)
	}
	return session, nil
}

// createHandoffOp returns the handoff and the destination's display name, which
// the caller folds into the instructions it shows.
func (s *Server) createHandoffOp(ctx context.Context, sourceSessionID string, provider model.Provider, destinationAccountID string) (model.Handoff, string, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	session, err := s.store.GetSession(ctx, sourceSessionID)
	if err != nil {
		return model.Handoff{}, "", failText(http.StatusNotFound, "source session not found")
	}
	if provider != model.ProviderClaude && provider != model.ProviderCodex {
		return model.Handoff{}, "", failText(http.StatusBadRequest, "destination provider must be claude or codex")
	}
	destination := "Codex"
	if provider == model.ProviderClaude {
		destination = "Claude"
		account, accountErr := s.store.GetAccount(ctx, destinationAccountID)
		if accountErr != nil || account.Status != model.AccountAuthenticated {
			return model.Handoff{}, "", failText(http.StatusBadRequest, "destination Claude account is not authenticated")
		}
		return s.continueClaude(ctx, session, account)
	}
	handoff, err := s.store.CreateHandoff(ctx, store.CreateHandoffParams{
		SourceSessionID: session.ID, DestinationProvider: provider,
		DestinationAccountID: destinationAccountID, WorkspacePath: session.WorkspacePath,
	})
	if err != nil {
		return model.Handoff{}, "", fail(http.StatusBadRequest, err)
	}
	return handoff, destination, nil
}

func (s *Server) installHooksOp(ctx context.Context) error {
	if err := install.InstallHooks(s.layout); err != nil {
		return fail(http.StatusInternalServerError, err)
	}
	accounts, _ := s.store.ListAccounts(ctx)
	for _, account := range accounts {
		if err := install.InstallClaudeHooks(account.ProfileDir, s.binary); err != nil {
			return fail(http.StatusInternalServerError, err)
		}
	}
	return nil
}
