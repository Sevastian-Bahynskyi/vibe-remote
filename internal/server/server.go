package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/checkpoint"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/install"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/paths"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
	systemstate "github.com/Sevastian-Bahynskyi/vibe-remote/internal/system"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	store   *store.Store
	claude  *claude.Manager
	layout  paths.Layout
	binary  string
	http    *http.Server
	started time.Time
	opMu    sync.Mutex
}

type Options struct {
	Store  *store.Store
	Claude *claude.Manager
	Layout paths.Layout
	Binary string
}

func New(options Options) (*Server, error) {
	if options.Store == nil || options.Claude == nil {
		return nil, errors.New("server requires store and Claude manager")
	}
	server := &Server{
		store: options.Store, claude: options.Claude, layout: options.Layout,
		binary: options.Binary, started: time.Now().UTC(),
	}
	mux := http.NewServeMux()
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("load dashboard assets: %w", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /{$}", server.index)
	mux.HandleFunc("GET /api/state", server.state)
	mux.HandleFunc("POST /api/accounts", server.addAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", server.removeAccount)
	mux.HandleFunc("POST /api/remote-sessions", server.createRemoteSession)
	mux.HandleFunc("PATCH /api/remote-sessions/{id}", server.updateRemoteSession)
	mux.HandleFunc("POST /api/remote-sessions/{id}/start", server.startRemoteSession)
	mux.HandleFunc("POST /api/remote-sessions/{id}/restart", server.restartRemoteSession)
	mux.HandleFunc("POST /api/remote-sessions/{id}/stop", server.stopRemoteSession)
	mux.HandleFunc("DELETE /api/remote-sessions/{id}", server.deleteRemoteSession)
	mux.HandleFunc("POST /api/auth/{id}/refresh", server.refreshAccount)
	mux.HandleFunc("POST /api/workspaces", server.addWorkspace)
	mux.HandleFunc("DELETE /api/workspaces/{id}", server.removeWorkspace)
	mux.HandleFunc("POST /api/handoffs", server.createHandoff)
	mux.HandleFunc("PATCH /api/sessions/{id}", server.updateSession)
	mux.HandleFunc("POST /api/install-hooks", server.installHooks)
	server.http = &http.Server{
		Addr:              paths.ListenAddr,
		Handler:           server.securityHeaders(server.requireMutationHeader(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Starting or restarting a remote session blocks until Claude registers
		// Remote Control, which a cold isolated profile can take well over a
		// minute to do. Keep this above the manager's ready timeout so a slow but
		// successful start still reaches the dashboard.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return server, nil
}

func (s *Server) ListenAndServe() error {
	listener, err := net.Listen("tcp4", paths.ListenAddr)
	if err != nil {
		return err
	}
	return s.http.Serve(listener)
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// Restore brings every remote session that should be running back up after the
// service restarts. One failing session must not block the others.
// Restore brings every slot that should be running back up. Claude serializes
// activation internally and a cold profile can take over a minute to register,
// so this is slow by nature and callers should not block the listener on it:
// see Server.RestoreInBackground.
//
// The lock is taken per slot rather than for the whole sweep so dashboard
// mutations can interleave, and each slot is re-read under that lock because
// the user may have changed or removed it while an earlier slot was starting.
func (s *Server) Restore(ctx context.Context) error {
	remotes, err := s.store.ListRemoteSessions(ctx)
	if err != nil {
		return err
	}
	var failures []string
	for _, listed := range remotes {
		if listed.Desired != model.DesiredRunning {
			continue
		}
		if failure := s.restoreOne(ctx, listed.ID); failure != "" {
			failures = append(failures, failure)
		}
		if ctx.Err() != nil {
			break
		}
	}
	if len(failures) > 0 {
		return errors.New("could not restore " + strings.Join(failures, "; "))
	}
	return nil
}

// restoreOne starts a single slot and reports a human-readable failure, or ""
// when the slot started or no longer wants to be running.
func (s *Server) restoreOne(ctx context.Context, id string) string {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	remote, err := s.store.GetRemoteSession(ctx, id)
	if err != nil {
		// Removed from the dashboard while an earlier slot was starting.
		return ""
	}
	if remote.Desired != model.DesiredRunning {
		return ""
	}
	account, accountErr := s.store.GetAccount(ctx, remote.AccountID)
	if accountErr != nil {
		return remote.Name + ": account unavailable"
	}
	if account.Status != model.AccountAuthenticated {
		return remote.Name + ": account is not authenticated"
	}
	if hookErr := install.InstallClaudeHooks(account.ProfileDir, s.binary); hookErr != nil {
		return remote.Name + ": " + hookErr.Error()
	}
	if startErr := s.claude.Activate(ctx, workerSpec(remote)); startErr != nil {
		return remote.Name + ": " + startErr.Error()
	}
	return ""
}

// RestoreInBackground runs Restore without blocking, reporting failures through
// report. The dashboard binds immediately and each slot reports its own state
// as it comes up, instead of the service going dark for the whole sweep.
func (s *Server) RestoreInBackground(ctx context.Context, report func(error)) {
	go func() {
		if err := s.Restore(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
	}()
}

func workerSpec(remote model.RemoteSession) claude.WorkerSpec {
	return claude.WorkerSpec{
		ID:              remote.ID,
		AccountID:       remote.AccountID,
		Workspace:       remote.WorkspacePath,
		Name:            remote.Name,
		ResumeSessionID: remote.ResumeSessionID,
	}
}

func (s *Server) index(response http.ResponseWriter, _ *http.Request) {
	data, err := assets.ReadFile("static/index.html")
	if err != nil {
		http.Error(response, "Dashboard unavailable", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = response.Write(data)
}

func (s *Server) state(response http.ResponseWriter, request *http.Request) {
	_, _ = s.store.CleanupClosedSessions(request.Context(), time.Now().Add(-30*24*time.Hour))
	accounts, err := s.store.ListAccounts(request.Context())
	if err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	workspaces, err := s.store.ListWorkspaces(request.Context())
	if err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	sessions, err := s.store.ListSessions(request.Context(), store.SessionFilter{Limit: 250})
	if err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	remotes, err := s.store.ListRemoteSessions(request.Context())
	if err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	s.decorateRemoteSessions(remotes)
	decorateAccountActivity(accounts, remotes)
	decorateSessions(sessions, accounts)
	health := systemstate.Inspect(request.Context(), s.layout.CodexHooks, s.layout.CodexHookVerified, s.layout.Binary)
	powerWarning := ""
	if !health.OnACPower {
		powerWarning = "This Mac is on battery and may become unreachable."
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"accounts":       accounts,
		"workspaces":     workspaces,
		"sessions":       sessions,
		"remoteSessions": remotes,
		"runningCount":   s.claude.RunningCount(),
		"system": map[string]any{
			"tailnetOnly":     health.TailnetOnly,
			"serveReady":      health.ServeReady,
			"funnelOff":       health.FunnelOff,
			"tailscaleOnline": health.TailscaleOnline,
			"tailscaleUrl":    health.TailscaleURL,
			"hooksHealthy":    health.CodexHooks,
			"hooksInstalled":  health.CodexHooksInstalled,
			"onAcPower":       health.OnACPower,
			"powerWarning":    powerWarning,
			"claudeInstalled": health.ClaudeBinary,
			"codexInstalled":  health.CodexBinary,
			"startedAt":       s.started,
		},
	})
}

// decorateRemoteSessions attaches live worker state to each stored slot.
func (s *Server) decorateRemoteSessions(remotes []model.RemoteSession) {
	for index := range remotes {
		remote := &remotes[index]
		remote.Worker = s.claude.Status(remote.ID)
		if remote.Worker.Name == "" {
			remote.Worker.Name = remote.Name
		}
	}
}

// decorateAccountActivity reports an account as active when at least one of its
// remote sessions is running. Several accounts can be active at once.
func decorateAccountActivity(accounts []model.Account, remotes []model.RemoteSession) {
	running := make(map[string]bool, len(remotes))
	for _, remote := range remotes {
		if remote.Worker.Running {
			running[remote.AccountID] = true
		}
	}
	for index := range accounts {
		accounts[index].Active = running[accounts[index].ID]
	}
}

func decorateSessions(sessions []model.Session, accounts []model.Account) {
	profiles := make(map[string]string, len(accounts))
	for _, account := range accounts {
		profiles[account.ID] = account.ProfileDir
	}
	activePaths := make(map[string]int)
	for _, session := range sessions {
		path := session.WorktreePath
		if path == "" {
			path = session.WorkspacePath
		}
		if session.State == model.SessionPrompted && path != "" {
			activePaths[path]++
		}
	}
	for index := range sessions {
		session := &sessions[index]
		path := session.WorktreePath
		if path == "" {
			path = session.WorkspacePath
		}
		session.SharedWorktree = activePaths[path] > 1
		if session.Provider == model.ProviderClaude {
			if profile := profiles[session.AccountID]; profile != "" {
				session.ResumeCommand = "env CLAUDE_CONFIG_DIR=" + shellQuote(profile) + " VIBE_REMOTE_ACCOUNT_ID=" + shellQuote(session.AccountID) + " claude --resume " + shellQuote(session.NativeSessionID)
				session.DesktopGuidance = "Run the resume command locally, then type /desktop to move this CLI session into Claude Desktop."
			}
		} else {
			session.ResumeCommand = "codex resume " + shellQuote(session.NativeSessionID)
		}
	}
}

func (s *Server) addAccount(response http.ResponseWriter, request *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(request, &body); err != nil || strings.TrimSpace(body.Email) == "" {
		writeError(response, errors.New("a valid email is required"), http.StatusBadRequest)
		return
	}
	if err := launchAccountLogin(s.binary, body.Email); err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]any{
		"message": "Official Claude sign-in opened on this Mac. The account will appear after authentication completes.",
	})
}

func (s *Server) removeAccount(response http.ResponseWriter, request *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	id := request.PathValue("id")
	deleteCheckpoints := request.URL.Query().Get("deleteCheckpoints") == "true"
	removeErr := error(nil)
	if deleteCheckpoints {
		removeErr = s.claude.RemoveProfile(request.Context(), id, false)
	} else {
		removeErr = s.claude.Remove(request.Context(), id, false)
	}
	if removeErr != nil {
		status := http.StatusBadRequest
		if errors.Is(removeErr, claude.ErrStopTimeout) {
			status = http.StatusConflict
		}
		writeError(response, removeErr, status)
		return
	}
	if deleteCheckpoints {
		if err := s.store.DeleteAccountAndSessions(request.Context(), id); err != nil {
			writeError(response, err, http.StatusInternalServerError)
			return
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": "Claude account removed."})
}

type remoteSessionRequest struct {
	Name            string `json:"name"`
	AccountID       string `json:"accountId"`
	WorkspaceID     string `json:"workspaceId"`
	ResumeSessionID string `json:"resumeSessionId"`
	Force           bool   `json:"force"`
}

func (s *Server) createRemoteSession(response http.ResponseWriter, request *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var body remoteSessionRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	account, workspace, err := s.resolveRouting(request.Context(), body.AccountID, body.WorkspaceID)
	if err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	resume, err := s.resolveResume(request.Context(), body.ResumeSessionID, account.ID, workspace.Path)
	if err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	existing, err := s.store.ListRemoteSessions(request.Context())
	if err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	remote, err := s.store.UpsertRemoteSession(request.Context(), model.RemoteSession{
		Name:            uniqueSessionName(body.Name, account.Email, workspace, existing, ""),
		AccountID:       account.ID,
		WorkspaceID:     workspace.ID,
		WorkspacePath:   workspace.Path,
		ResumeSessionID: resume,
		Desired:         model.DesiredRunning,
	})
	if err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	if err := s.startRemote(request.Context(), remote, body.Force); err != nil {
		_ = s.store.DeleteRemoteSession(request.Context(), remote.ID)
		writeError(response, err, startFailureStatus(err))
		return
	}
	_ = s.store.SelectWorkspace(request.Context(), workspace.ID)
	writeJSON(response, http.StatusCreated, map[string]any{
		"remoteSession": remote,
		"message":       remote.Name + " is ready for Claude Remote Control.",
	})
}

// updateRemoteSession moves a session to another account, workspace, or
// conversation and restarts it so the change takes effect.
func (s *Server) updateRemoteSession(response http.ResponseWriter, request *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var body remoteSessionRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	remote, err := s.store.GetRemoteSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, errors.New("remote session not found"), http.StatusNotFound)
		return
	}
	accountID := remote.AccountID
	if strings.TrimSpace(body.AccountID) != "" {
		accountID = body.AccountID
	}
	workspaceID := remote.WorkspaceID
	if strings.TrimSpace(body.WorkspaceID) != "" {
		workspaceID = body.WorkspaceID
	}
	account, workspace, err := s.resolveRouting(request.Context(), accountID, workspaceID)
	if err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	resume, err := s.resolveResume(request.Context(), body.ResumeSessionID, account.ID, workspace.Path)
	if err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	if err := s.stopRemote(request.Context(), remote, body.Force); err != nil {
		writeError(response, err, stopFailureStatus(err))
		return
	}
	existing, err := s.store.ListRemoteSessions(request.Context())
	if err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
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
	remote, err = s.store.UpsertRemoteSession(request.Context(), remote)
	if err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	if err := s.startRemote(request.Context(), remote, true); err != nil {
		writeError(response, err, startFailureStatus(err))
		return
	}
	_ = s.store.SelectWorkspace(request.Context(), workspace.ID)
	writeJSON(response, http.StatusOK, map[string]any{
		"remoteSession": remote,
		"message":       remote.Name + " restarted with the new settings.",
	})
}

func (s *Server) startRemoteSession(response http.ResponseWriter, request *http.Request) {
	s.remoteSessionLifecycle(response, request, false)
}

func (s *Server) restartRemoteSession(response http.ResponseWriter, request *http.Request) {
	s.remoteSessionLifecycle(response, request, true)
}

func (s *Server) remoteSessionLifecycle(response http.ResponseWriter, request *http.Request, restart bool) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var body remoteSessionRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	remote, err := s.store.GetRemoteSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, errors.New("remote session not found"), http.StatusNotFound)
		return
	}
	if restart {
		if err := s.stopRemote(request.Context(), remote, body.Force); err != nil {
			writeError(response, err, stopFailureStatus(err))
			return
		}
	}
	remote, err = s.store.SetRemoteSessionDesired(request.Context(), remote.ID, model.DesiredRunning)
	if err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	if err := s.startRemote(request.Context(), remote, restart || body.Force); err != nil {
		writeError(response, err, startFailureStatus(err))
		return
	}
	verb := "started"
	if restart {
		verb = "restarted"
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"remoteSession": remote,
		"message":       remote.Name + " " + verb + ".",
	})
}

func (s *Server) stopRemoteSession(response http.ResponseWriter, request *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var body remoteSessionRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	remote, err := s.store.GetRemoteSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, errors.New("remote session not found"), http.StatusNotFound)
		return
	}
	if err := s.stopRemote(request.Context(), remote, body.Force); err != nil {
		writeError(response, err, stopFailureStatus(err))
		return
	}
	if _, err := s.store.SetRemoteSessionDesired(request.Context(), remote.ID, model.DesiredStopped); err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": remote.Name + " stopped. Its checkpoints are kept."})
}

func (s *Server) deleteRemoteSession(response http.ResponseWriter, request *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	remote, err := s.store.GetRemoteSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, errors.New("remote session not found"), http.StatusNotFound)
		return
	}
	if err := s.stopRemote(request.Context(), remote, true); err != nil {
		writeError(response, err, stopFailureStatus(err))
		return
	}
	if err := s.store.DeleteRemoteSession(request.Context(), remote.ID); err != nil {
		writeError(response, err, http.StatusNotFound)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": remote.Name + " removed. Its checkpoints are kept."})
}

// startRemote installs the account's hooks and hands the slot to the Claude
// manager. Other slots keep running.
func (s *Server) startRemote(ctx context.Context, remote model.RemoteSession, force bool) error {
	account, err := s.store.GetAccount(ctx, remote.AccountID)
	if err != nil {
		return errors.New("account not found")
	}
	if err := install.InstallClaudeHooks(account.ProfileDir, s.binary); err != nil {
		return err
	}
	spec := workerSpec(remote)
	spec.Force = force
	return s.claude.Activate(ctx, spec)
}

// stopRemote stops one slot, checkpointing any Claude turn that was still
// running so no captured work is lost.
func (s *Server) stopRemote(ctx context.Context, remote model.RemoteSession, force bool) error {
	worker := s.claude.Status(remote.ID)
	if worker.State == "stopped" && !worker.Running {
		return nil
	}
	active := s.activePromptedSessions(ctx, remote.AccountID, remote.WorkspacePath)
	if err := s.claude.Deactivate(ctx, remote.ID, force); err != nil {
		if errors.Is(err, claude.ErrStopTimeout) && !force {
			return errors.New("this session has an unfinished Claude turn; confirm a forced stop")
		}
		return err
	}
	for _, session := range active {
		if err := s.markSessionInterrupted(ctx, session); err != nil {
			return errors.New("could not checkpoint the stopped Claude turn")
		}
	}
	return nil
}

func (s *Server) resolveRouting(ctx context.Context, accountID, workspaceID string) (model.Account, model.Workspace, error) {
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return model.Account{}, model.Workspace{}, errors.New("account not found")
	}
	if account.Status != model.AccountAuthenticated {
		return model.Account{}, model.Workspace{}, errors.New("this Claude account is not authenticated")
	}
	workspace, err := s.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return model.Account{}, model.Workspace{}, errors.New("workspace not found")
	}
	return account, workspace, nil
}

// resolveResume accepts a stored checkpoint ID or a native Claude session ID and
// returns the native ID to resume. An empty value starts a new conversation.
func (s *Server) resolveResume(ctx context.Context, requested, accountID, workspacePath string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", nil
	}
	session, err := s.store.GetSession(ctx, requested)
	if err != nil {
		return "", errors.New("that checkpoint is no longer available")
	}
	if session.Provider != model.ProviderClaude {
		return "", errors.New("only Claude checkpoints can be resumed here")
	}
	if session.AccountID != accountID {
		return "", errors.New("that conversation belongs to another Claude account")
	}
	if canonicalWorkspace(session.WorkspacePath) != canonicalWorkspace(workspacePath) {
		return "", errors.New("that conversation belongs to another workspace")
	}
	return session.NativeSessionID, nil
}

func canonicalWorkspace(path string) string {
	return filepath.Clean(strings.TrimSpace(path))
}

// uniqueSessionName keeps every Remote Control name distinct so the phone can
// tell parallel sessions apart.
func uniqueSessionName(requested, email string, workspace model.Workspace, existing []model.RemoteSession, skipID string) string {
	base := strings.Join(strings.Fields(requested), " ")
	if base == "" {
		label := workspace.Label
		if strings.TrimSpace(label) == "" {
			label = filepath.Base(workspace.Path)
		}
		base = "Vibe Remote · " + email + " · " + label
	}
	if len(base) > 90 {
		base = strings.TrimSpace(base[:90])
	}
	taken := make(map[string]bool, len(existing))
	for _, remote := range existing {
		if remote.ID != skipID {
			taken[remote.Name] = true
		}
	}
	candidate := base
	for attempt := 2; taken[candidate]; attempt++ {
		candidate = fmt.Sprintf("%s (%d)", base, attempt)
	}
	return candidate
}

func startFailureStatus(err error) int {
	if errors.Is(err, claude.ErrStopTimeout) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func stopFailureStatus(err error) int {
	if errors.Is(err, claude.ErrStopTimeout) || strings.Contains(err.Error(), "forced stop") {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func (s *Server) refreshAccount(response http.ResponseWriter, request *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	account, err := s.claude.Refresh(request.Context(), request.PathValue("id"))
	if err != nil {
		if launchErr := launchAccountRelogin(s.binary, request.PathValue("id")); launchErr != nil {
			writeError(response, launchErr, http.StatusInternalServerError)
			return
		}
		writeJSON(response, http.StatusAccepted, map[string]any{"message": "Official Claude sign-in reopened on this Mac."})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"account": account, "message": "Claude authentication verified."})
}

func (s *Server) addWorkspace(response http.ResponseWriter, request *http.Request) {
	var body struct {
		Label string `json:"label"`
		Path  string `json:"path"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	path, err := validateWorkspace(body.Path)
	if err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	workspace, err := s.store.UpsertWorkspace(request.Context(), model.Workspace{
		Label: strings.TrimSpace(body.Label), Path: path, Selected: true,
	})
	if err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{"workspace": workspace, "message": "Workspace added."})
}

func (s *Server) removeWorkspace(response http.ResponseWriter, request *http.Request) {
	if err := s.store.DeleteWorkspace(request.Context(), request.PathValue("id")); err != nil {
		writeError(response, err, http.StatusNotFound)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": "Workspace removed; project files were not changed."})
}

func (s *Server) createHandoff(response http.ResponseWriter, request *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var body struct {
		SourceSessionID      string         `json:"sourceSessionId"`
		DestinationProvider  model.Provider `json:"destinationProvider"`
		DestinationAccountID string         `json:"destinationAccountId"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	session, err := s.store.GetSession(request.Context(), body.SourceSessionID)
	if err != nil {
		writeError(response, errors.New("source session not found"), http.StatusNotFound)
		return
	}
	if body.DestinationProvider != model.ProviderClaude && body.DestinationProvider != model.ProviderCodex {
		writeError(response, errors.New("destination provider must be claude or codex"), http.StatusBadRequest)
		return
	}
	if body.DestinationProvider == model.ProviderClaude {
		account, accountErr := s.store.GetAccount(request.Context(), body.DestinationAccountID)
		if accountErr != nil || account.Status != model.AccountAuthenticated {
			writeError(response, errors.New("destination Claude account is not authenticated"), http.StatusBadRequest)
			return
		}
		if err := s.ensureRemoteSession(request.Context(), account, session.WorkspacePath); err != nil {
			writeError(response, err, http.StatusConflict)
			return
		}
	}
	handoff, err := s.store.CreateHandoff(request.Context(), store.CreateHandoffParams{
		SourceSessionID: session.ID, DestinationProvider: body.DestinationProvider,
		DestinationAccountID: body.DestinationAccountID, WorkspacePath: session.WorkspacePath,
	})
	if err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	destination := "Codex"
	if body.DestinationProvider == model.ProviderClaude {
		destination = "Claude"
	}
	writeJSON(response, http.StatusCreated, map[string]any{
		"handoff":      handoff,
		"instructions": "Handoff ready for 48 hours. Open " + destination + " in this workspace and type continue.",
	})
}

func (s *Server) installHooks(response http.ResponseWriter, request *http.Request) {
	if err := install.InstallHooks(s.layout); err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	accounts, _ := s.store.ListAccounts(request.Context())
	for _, account := range accounts {
		if err := install.InstallClaudeHooks(account.ProfileDir, s.binary); err != nil {
			writeError(response, err, http.StatusInternalServerError)
			return
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": "Claude and Codex checkpoint hooks installed."})
}

func (s *Server) updateSession(response http.ResponseWriter, request *http.Request) {
	var body struct {
		Title  string `json:"title"`
		Pinned bool   `json:"pinned"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	session, err := s.store.UpdateSessionMetadata(request.Context(), request.PathValue("id"), body.Title, body.Pinned)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(response, err, status)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"session": session, "message": "Checkpoint updated."})
}

func (s *Server) activePromptedSessions(ctx context.Context, accountID, workspacePath string) []model.Session {
	if accountID == "" || workspacePath == "" {
		return nil
	}
	sessions, err := s.store.ListSessions(ctx, store.SessionFilter{Provider: model.ProviderClaude, AccountID: accountID, WorkspacePath: workspacePath, Limit: 100})
	if err != nil {
		return nil
	}
	active := make([]model.Session, 0)
	for _, session := range sessions {
		if session.State == model.SessionPrompted {
			active = append(active, session)
		}
	}
	return active
}

func (s *Server) markSessionInterrupted(ctx context.Context, session model.Session) error {
	snapshot, err := checkpoint.NewGitCapturer().Capture(ctx, session.WorkspacePath)
	if err != nil {
		snapshot = model.GitSnapshot{CapturedAt: time.Now().UTC()}
	}
	return s.store.InterruptSession(ctx, session.ID, snapshot, time.Now().UTC())
}

// ensureRemoteSession guarantees the destination account has a running Remote
// Control session in this workspace, reusing one when it already exists.
func (s *Server) ensureRemoteSession(ctx context.Context, account model.Account, workspacePath string) error {
	workspace, err := s.store.GetWorkspaceByPath(ctx, workspacePath)
	if err != nil {
		workspace, err = s.store.UpsertWorkspace(ctx, model.Workspace{Label: filepath.Base(workspacePath), Path: workspacePath, Selected: true})
		if err != nil {
			return err
		}
	}
	remotes, err := s.store.ListRemoteSessions(ctx)
	if err != nil {
		return err
	}
	for _, remote := range remotes {
		if remote.AccountID != account.ID || canonicalWorkspace(remote.WorkspacePath) != canonicalWorkspace(workspace.Path) {
			continue
		}
		if s.claude.Status(remote.ID).Running {
			return nil
		}
		if _, err := s.store.SetRemoteSessionDesired(ctx, remote.ID, model.DesiredRunning); err != nil {
			return err
		}
		return s.startRemote(ctx, remote, false)
	}
	remote, err := s.store.UpsertRemoteSession(ctx, model.RemoteSession{
		Name:          uniqueSessionName("", account.Email, workspace, remotes, ""),
		AccountID:     account.ID,
		WorkspaceID:   workspace.ID,
		WorkspacePath: workspace.Path,
		Desired:       model.DesiredRunning,
	})
	if err != nil {
		return err
	}
	if err := s.startRemote(ctx, remote, false); err != nil {
		_ = s.store.DeleteRemoteSession(ctx, remote.ID)
		return err
	}
	return nil
}

func selectedWorkspace(workspaces []model.Workspace) model.Workspace {
	for _, workspace := range workspaces {
		if workspace.Selected {
			return workspace
		}
	}
	return model.Workspace{}
}

func validateWorkspace(input string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(input))
	if clean == "." || !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return "", errors.New("workspace must be an absolute, non-root directory")
	}
	home, err := os.UserHomeDir()
	if err != nil || clean == filepath.Clean(home) {
		return "", errors.New("home directory cannot be used as a workspace")
	}
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		return "", errors.New("workspace directory does not exist")
	}
	return clean, nil
}

func launchAccountLogin(binary, email string) error {
	command := shellQuote(binary) + " account-add --email " + shellQuote(strings.TrimSpace(email)) + "; printf '\\nSign-in finished. You may close this window.\\n'; read -k 1"
	return launchTerminalCommand(command)
}

func launchAccountRelogin(binary, accountID string) error {
	command := shellQuote(binary) + " account-login --id " + shellQuote(accountID) + "; printf '\\nSign-in finished. You may close this window.\\n'; read -k 1"
	return launchTerminalCommand(command)
}

func launchTerminalCommand(command string) error {
	script := `tell application "Terminal" to do script ` + appleScriptQuote(command)
	output, err := exec.Command("/usr/bin/osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("open official Claude login: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func appleScriptQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func decodeJSON(request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid request")
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeError(response http.ResponseWriter, err error, status int) {
	message := strings.TrimSpace(err.Error())
	if status >= http.StatusInternalServerError {
		message = "Internal operation failed. Check the local Vibe Remote service log."
	} else {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			message = strings.ReplaceAll(message, home, "~")
		}
		message = strings.Join(strings.Fields(message), " ")
		if len(message) > 240 {
			message = message[:239] + "…"
		}
	}
	if message == "" {
		message = "Request failed."
	}
	writeJSON(response, status, map[string]string{"error": message})
}

func (s *Server) requireMutationHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead && request.Header.Get("X-Vibe-Remote") != "1" {
			writeError(response, errors.New("missing mutation authorization header"), http.StatusForbidden)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(response, request)
	})
}
