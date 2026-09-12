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
	"net/url"
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
	views   *views
	opMu    sync.Mutex

	// System health shells out to pmset and tailscale, so it is cached briefly:
	// the dashboard asks for it on a timer and a burst of requests must not turn
	// into a burst of subprocesses on a Mac this service deliberately keeps awake.
	healthMu    sync.Mutex
	healthValue systemstate.Health
	healthAt    time.Time
}

// healthTTL is short enough that a change the user just made shows up on the
// next refresh, and long enough that polling costs nothing.
const healthTTL = 10 * time.Second

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
	// Parsed once, at startup: a broken template must fail the binary rather than
	// one request on a phone.
	parsed, err := parseViews()
	if err != nil {
		return nil, err
	}
	server := &Server{
		store: options.Store, claude: options.Claude, layout: options.Layout,
		binary: options.Binary, started: time.Now().UTC(), views: parsed,
	}
	mux := http.NewServeMux()
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("load dashboard assets: %w", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /{$}", server.dashboard)
	mux.HandleFunc("GET /api/state", server.state)
	mux.HandleFunc("POST /api/accounts", server.addAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", server.removeAccount)
	mux.HandleFunc("POST /api/remote-sessions", server.createRemoteSession)
	mux.HandleFunc("PATCH /api/remote-sessions/{id}", server.updateRemoteSession)
	mux.HandleFunc("POST /api/remote-sessions/{id}/start", server.startRemoteSession)
	mux.HandleFunc("POST /api/remote-sessions/{id}/restart", server.restartRemoteSession)
	mux.HandleFunc("POST /api/remote-sessions/{id}/stop", server.stopRemoteSession)
	mux.HandleFunc("POST /api/remote-sessions/{id}/open-desktop", server.openRemoteSessionDesktop)
	mux.HandleFunc("DELETE /api/remote-sessions/{id}", server.deleteRemoteSession)
	mux.HandleFunc("POST /api/auth/{id}/refresh", server.refreshAccount)
	mux.HandleFunc("POST /api/workspaces", server.addWorkspace)
	mux.HandleFunc("DELETE /api/workspaces/{id}", server.removeWorkspace)
	mux.HandleFunc("POST /api/handoffs", server.createHandoff)
	mux.HandleFunc("PATCH /api/sessions/{id}", server.updateSession)
	mux.HandleFunc("POST /api/install-hooks", server.installHooks)

	// The dashboard. These render HTML; /api/* above stays a JSON API and is no
	// longer what the dashboard talks to.
	//
	// Every screen is served by the one route registered above, addressed by
	// ?screen=, so the document URL never gains a directory level and the page's
	// relative URLs keep resolving against the mount root — see screenURL. The
	// routes below are only ever fetched by htmx, never shown in the address bar,
	// so they are free to be paths.
	mux.HandleFunc("GET /ui/fragments/sessions", server.uiFragmentSessions)
	mux.HandleFunc("GET /ui/fragments/alerts", server.uiFragmentAlerts)
	mux.HandleFunc("GET /ui/fragments/session/{id}", server.uiFragmentSession)
	mux.HandleFunc("GET /ui/fragments/conversation-options", server.uiFragmentConversationOptions)

	mux.HandleFunc("POST /ui/sessions", server.uiCreateSession)
	mux.HandleFunc("POST /ui/sessions/{id}", server.uiUpdateSession)
	mux.HandleFunc("POST /ui/sessions/{id}/start", server.uiSessionLifecycle("start"))
	mux.HandleFunc("POST /ui/sessions/{id}/restart", server.uiSessionLifecycle("restart"))
	mux.HandleFunc("POST /ui/sessions/{id}/stop", server.uiSessionLifecycle("stop"))
	mux.HandleFunc("POST /ui/sessions/{id}/open-desktop", server.uiOpenDesktop)
	mux.HandleFunc("POST /ui/sessions/{id}/delete", server.uiDeleteSession)
	mux.HandleFunc("POST /ui/accounts", server.uiAddAccount)
	mux.HandleFunc("POST /ui/accounts/{id}/refresh", server.uiRefreshAccount)
	mux.HandleFunc("POST /ui/accounts/{id}/delete", server.uiDeleteAccount)
	mux.HandleFunc("POST /ui/workspaces", server.uiAddWorkspace)
	mux.HandleFunc("POST /ui/workspaces/{id}/delete", server.uiDeleteWorkspace)
	mux.HandleFunc("POST /ui/conversations/{id}", server.uiUpdateConversation)
	mux.HandleFunc("POST /ui/handoffs", server.uiCreateHandoff)
	mux.HandleFunc("POST /ui/install-hooks", server.uiInstallHooks)
	mux.HandleFunc("POST /ui/retention", server.uiRetention)
	server.http = &http.Server{
		Addr:              paths.ListenAddr,
		Handler:           server.securityHeaders(server.requireMutationHeader(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Starting a session no longer waits for Claude to register Remote
		// Control, so no request is on the hook for a cold profile's startup any
		// more. What is left is bounded by a subprocess or two (an account status
		// check, a Tailscale query), and a request outliving this is a hang worth
		// surfacing rather than waiting out.
		WriteTimeout: 45 * time.Second,
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

func (s *Server) state(response http.ResponseWriter, request *http.Request) {
	// Retention also runs on a timer (see StartMaintenance); this call is kept so
	// the JSON route behaves exactly as it always has.
	s.sweepClosedSessions(request.Context())
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	health := state.Health
	writeJSON(response, http.StatusOK, map[string]any{
		"accounts":       state.Accounts,
		"workspaces":     state.Workspaces,
		"sessions":       state.Sessions,
		"remoteSessions": state.Remotes,
		"runningCount":   state.Running,
		"system": map[string]any{
			"tailnetOnly":     health.TailnetOnly,
			"serveReady":      health.ServeReady,
			"funnelOff":       health.FunnelOff,
			"tailscaleOnline": health.TailscaleOnline,
			"tailscaleUrl":    health.TailscaleURL,
			"hooksHealthy":    health.CodexHooks,
			"hooksInstalled":  health.CodexHooksInstalled,
			"onAcPower":       health.OnACPower,
			"powerWarning":    state.PowerWarning,
			"claudeInstalled": health.ClaudeBinary,
			"codexInstalled":  health.CodexBinary,
			"claudeDesktop":   health.ClaudeDesktop,
			"onThisMac":       state.OnThisMac,
			"startedAt":       state.StartedAt,
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
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, errors.New("a valid email is required"), http.StatusBadRequest)
		return
	}
	if err := s.addAccountOp(body.Email); err != nil {
		writeError(response, err, statusOf(err))
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]any{
		"message": "Official Claude sign-in opened on this Mac. The account will appear after authentication completes.",
	})
}

func (s *Server) removeAccount(response http.ResponseWriter, request *http.Request) {
	deleteCheckpoints := request.URL.Query().Get("deleteCheckpoints") == "true"
	if err := s.removeAccountOp(request.Context(), request.PathValue("id"), deleteCheckpoints); err != nil {
		writeError(response, err, statusOf(err))
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": "Claude account removed."})
}

// remoteSessionRequest carries a slot edit. ResumeSessionID is a pointer so an
// omitted field keeps the conversation the slot is already linked to, while an
// explicit empty string is the deliberate "new conversation" reset. Without that
// distinction, renaming a slot would silently detach its live thread.
type remoteSessionRequest struct {
	Name            string  `json:"name"`
	AccountID       string  `json:"accountId"`
	WorkspaceID     string  `json:"workspaceId"`
	ResumeSessionID *string `json:"resumeSessionId"`
	Force           bool    `json:"force"`
}

func (s *Server) createRemoteSession(response http.ResponseWriter, request *http.Request) {
	var body remoteSessionRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	remote, err := s.createSlotOp(request.Context(), body)
	if err != nil {
		writeError(response, err, statusOf(err))
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{
		"remoteSession": remote,
		"message":       remote.Name + " is ready for Claude Remote Control.",
	})
}

// updateRemoteSession moves a session to another account, workspace, or
// conversation and restarts it so the change takes effect.
func (s *Server) updateRemoteSession(response http.ResponseWriter, request *http.Request) {
	var body remoteSessionRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	remote, err := s.updateSlotOp(request.Context(), request.PathValue("id"), body)
	if err != nil {
		writeError(response, err, statusOf(err))
		return
	}
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
	var body remoteSessionRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	remote, err := s.lifecycleSlotOp(request.Context(), request.PathValue("id"), restart, body.Force)
	if err != nil {
		writeError(response, err, statusOf(err))
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
	var body remoteSessionRequest
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	remote, err := s.stopSlotOp(request.Context(), request.PathValue("id"), body.Force)
	if err != nil {
		writeError(response, err, statusOf(err))
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": remote.Name + " stopped. Its checkpoints are kept."})
}

// openRemoteSessionDesktop hands a running session's Remote Control link to
// Claude Desktop on this Mac. The tailnet is refused because the app would open
// here, not on the phone that asked.
func (s *Server) openRemoteSessionDesktop(response http.ResponseWriter, request *http.Request) {
	remote, err := s.openDesktopOp(request.Context(), request.PathValue("id"), isLocalRequest(request))
	if err != nil {
		writeError(response, err, statusOf(err))
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": remote.Name + " opened in Claude Desktop."})
}

// isLocalRequest reports whether the dashboard is being used on this Mac.
// Tailscale Serve proxies to the same loopback listener, so a loopback peer
// alone proves nothing; a proxied request always carries forwarding headers.
func isLocalRequest(request *http.Request) bool {
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Tailscale-User-Login"} {
		if request.Header.Get(header) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// isClaudeRemoteURL guards what is handed to `open`, so only a Remote Control
// link Claude itself printed can reach the desktop app.
func isClaudeRemoteURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	if parsed.Hostname() != "claude.ai" && parsed.Hostname() != "claude.com" {
		return false
	}
	return parsed.Path == "/code" || strings.HasPrefix(parsed.Path, "/code/")
}

func claudeDesktopDeepLink(value string) (string, bool) {
	if !isClaudeRemoteURL(value) {
		return "", false
	}
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	sessionID, ok := strings.CutPrefix(parsed.Path, "/code/")
	if !ok || (!strings.HasPrefix(sessionID, "cse_") && !strings.HasPrefix(sessionID, "session_")) {
		return "", false
	}
	for _, character := range sessionID {
		if character != '_' && character != '-' && (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
			return "", false
		}
	}
	return (&url.URL{Scheme: "claude", Host: "claude.ai", Path: "/code/" + sessionID}).String(), true
}

func (s *Server) deleteRemoteSession(response http.ResponseWriter, request *http.Request) {
	remote, err := s.deleteSlotOp(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, err, statusOf(err))
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

// restatedError presents its own message while keeping the underlying sentinel
// reachable through errors.Is. Callers that must decide whether to offer a
// forced retry read the sentinel; the user reads the sentence. Without this the
// two are the same string, and rewording the sentence silently changes the
// status code.
type restatedError struct {
	message string
	cause   error
}

func (e *restatedError) Error() string { return e.message }
func (e *restatedError) Unwrap() error { return e.cause }

func restate(message string, cause error) error {
	return &restatedError{message: message, cause: cause}
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
			return restate("this session has an unfinished Claude turn; confirm a forced stop", err)
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

// resolveResume turns a requested conversation into the native Claude ID the
// worker resumes. A request names a stored checkpoint; an omitted request keeps
// kept, the conversation the slot is already linked to; an empty request is the
// explicit reset to a fresh conversation.
func (s *Server) resolveResume(ctx context.Context, requested *string, kept, accountID, workspacePath string) (string, error) {
	if requested == nil {
		if kept != "" && !model.ValidResumeSessionID(kept) {
			return "", errors.New("this session's stored conversation is unreadable")
		}
		return kept, nil
	}
	trimmed := strings.TrimSpace(*requested)
	if trimmed == "" {
		return "", nil
	}
	session, err := s.store.GetSession(ctx, trimmed)
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
	if errors.Is(err, claude.ErrStopTimeout) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func (s *Server) refreshAccount(response http.ResponseWriter, request *http.Request) {
	account, reopened, err := s.refreshAccountOp(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, err, statusOf(err))
		return
	}
	if reopened {
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
	workspace, err := s.addWorkspaceOp(request.Context(), body.Label, body.Path)
	if err != nil {
		writeError(response, err, statusOf(err))
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{"workspace": workspace, "message": "Workspace added."})
}

func (s *Server) removeWorkspace(response http.ResponseWriter, request *http.Request) {
	if err := s.removeWorkspaceOp(request.Context(), request.PathValue("id")); err != nil {
		writeError(response, err, statusOf(err))
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": "Workspace removed; project files were not changed."})
}

func (s *Server) createHandoff(response http.ResponseWriter, request *http.Request) {
	var body struct {
		SourceSessionID      string         `json:"sourceSessionId"`
		DestinationProvider  model.Provider `json:"destinationProvider"`
		DestinationAccountID string         `json:"destinationAccountId"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	handoff, destination, err := s.createHandoffOp(request.Context(), body.SourceSessionID, body.DestinationProvider, body.DestinationAccountID)
	if err != nil {
		writeError(response, err, statusOf(err))
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{
		"handoff":      handoff,
		"instructions": handoffInstructions(body.DestinationProvider, destination),
	})
}

func (s *Server) installHooks(response http.ResponseWriter, request *http.Request) {
	if err := s.installHooksOp(request.Context()); err != nil {
		writeError(response, err, statusOf(err))
		return
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
	session, err := s.updateSessionOp(request.Context(), request.PathValue("id"), body.Title, body.Pinned)
	if err != nil {
		writeError(response, err, statusOf(err))
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

// errorMessage applies the dashboard's disclosure rules: a 5xx says nothing
// specific, anything else has the home directory collapsed to ~, whitespace
// folded, and is capped at 240 characters. Both the JSON API and the rendered
// dashboard go through here so the two can never redact differently.
func errorMessage(err error, status int) string {
	if status >= http.StatusInternalServerError {
		return "Internal operation failed. Check the local Vibe Remote service log."
	}
	message := strings.TrimSpace(err.Error())
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		message = strings.ReplaceAll(message, home, "~")
	}
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 240 {
		message = message[:239] + "…"
	}
	if message == "" {
		return "Request failed."
	}
	return message
}

func writeError(response http.ResponseWriter, err error, status int) {
	writeJSON(response, status, map[string]string{"error": errorMessage(err, status)})
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
