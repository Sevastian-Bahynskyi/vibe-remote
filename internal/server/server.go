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
	mux.HandleFunc("POST /api/accounts/{id}/activate", server.activateAccount)
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
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
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

func (s *Server) Restore(ctx context.Context) error {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	workspaces, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	workspace := selectedWorkspace(workspaces)
	if workspace.Path == "" {
		return nil
	}
	for _, account := range accounts {
		if account.Active && account.Status == model.AccountAuthenticated {
			if err := install.InstallClaudeHooks(account.ProfileDir, s.binary); err != nil {
				return err
			}
			return s.claude.Activate(ctx, account.ID, workspace.Path, "Vibe Remote · "+account.Email)
		}
	}
	return nil
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
	decorateSessions(sessions, accounts)
	health := systemstate.Inspect(request.Context(), s.layout.CodexHooks, s.layout.CodexHookVerified, s.layout.Binary)
	powerWarning := ""
	if !health.OnACPower {
		powerWarning = "This Mac is on battery and may become unreachable."
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"accounts":   accounts,
		"workspaces": workspaces,
		"sessions":   sessions,
		"worker":     s.claude.Status(),
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

func (s *Server) activateAccount(response http.ResponseWriter, request *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	var body struct {
		WorkspaceID string `json:"workspaceId"`
		Force       bool   `json:"force"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(response, err, http.StatusBadRequest)
		return
	}
	workspace, err := s.store.GetWorkspace(request.Context(), body.WorkspaceID)
	if err != nil {
		writeError(response, errors.New("workspace not found"), http.StatusNotFound)
		return
	}
	worker := s.claude.Status()
	activeSessions := s.activePromptedSessions(request.Context(), worker.AccountID, worker.WorkspacePath)
	hasActiveTurn := len(activeSessions) > 0
	if !body.Force && hasActiveTurn {
		if err := s.claude.Deactivate(request.Context(), false); err != nil {
			writeError(response, errors.New("the current Claude turn did not stop gracefully; confirm a forced switch"), http.StatusConflict)
			return
		}
		for _, session := range s.activePromptedSessions(request.Context(), worker.AccountID, worker.WorkspacePath) {
			if err := s.markSessionInterrupted(request.Context(), session); err != nil {
				writeError(response, errors.New("could not checkpoint the stopped Claude turn"), http.StatusInternalServerError)
				return
			}
		}
	}
	if body.Force && hasActiveTurn {
		for _, session := range activeSessions {
			if err := s.markSessionInterrupted(request.Context(), session); err != nil {
				writeError(response, err, http.StatusInternalServerError)
				return
			}
		}
	}
	if body.Force {
		if err := s.claude.Deactivate(request.Context(), true); err != nil {
			writeError(response, err, http.StatusConflict)
			return
		}
	}
	account, err := s.store.GetAccount(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, errors.New("account not found"), http.StatusNotFound)
		return
	}
	if err := install.InstallClaudeHooks(account.ProfileDir, s.binary); err != nil {
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	if err := s.claude.Activate(request.Context(), account.ID, workspace.Path, "Vibe Remote · "+account.Email); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, claude.ErrStopTimeout) {
			status = http.StatusConflict
		}
		writeError(response, err, status)
		return
	}
	if err := s.markActive(request.Context(), account.ID, workspace.ID); err != nil {
		_ = s.claude.Deactivate(context.Background(), true)
		writeError(response, err, http.StatusInternalServerError)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"message": account.Email + " is ready for Claude Remote Control."})
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
		worker := s.claude.Status()
		if active := s.activePromptedSessions(request.Context(), worker.AccountID, worker.WorkspacePath); len(active) > 0 {
			writeError(response, errors.New("the current Claude account has an unfinished turn; finish it or switch with force first"), http.StatusConflict)
			return
		}
		if err := install.InstallClaudeHooks(account.ProfileDir, s.binary); err != nil {
			writeError(response, err, http.StatusInternalServerError)
			return
		}
		if err := s.claude.Activate(request.Context(), account.ID, session.WorkspacePath, "Vibe Remote · "+account.Email); err != nil {
			writeError(response, err, http.StatusConflict)
			return
		}
		if err := s.markActiveByPath(request.Context(), account.ID, session.WorkspacePath); err != nil {
			_ = s.claude.Deactivate(context.Background(), true)
			writeError(response, err, http.StatusInternalServerError)
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

func (s *Server) markActive(ctx context.Context, accountID, workspaceID string) error {
	return s.store.SetActiveRouting(ctx, accountID, workspaceID)
}

func (s *Server) markActiveByPath(ctx context.Context, accountID, workspacePath string) error {
	workspace, err := s.store.GetWorkspaceByPath(ctx, workspacePath)
	if err != nil {
		workspace, err = s.store.UpsertWorkspace(ctx, model.Workspace{Label: filepath.Base(workspacePath), Path: workspacePath, Selected: true})
		if err != nil {
			return err
		}
	}
	return s.markActive(ctx, accountID, workspace.ID)
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
