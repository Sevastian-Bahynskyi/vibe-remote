package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/paths"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func TestDashboardWorksBehindTailscalePathPrefix(t *testing.T) {
	t.Parallel()
	service := newTestServer(t)
	proxy := http.NewServeMux()
	proxy.Handle(paths.DashboardPath+"/", http.StripPrefix(paths.DashboardPath, service.http.Handler))
	httpServer := httptest.NewServer(proxy)
	defer httpServer.Close()

	for _, target := range []string{
		httpServer.URL + paths.DashboardPath + "/",
		httpServer.URL + paths.DashboardPath + "/static/app.js",
		httpServer.URL + paths.DashboardPath + "/api/state",
	} {
		response, err := http.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, response.StatusCode, body)
		}
	}

	response, err := http.Get(httpServer.URL + paths.DashboardPath + "/")
	if err != nil {
		t.Fatal(err)
	}
	index, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `href="static/styles.css"`) || !strings.Contains(string(index), `src="static/app.js"`) {
		t.Fatal("dashboard assets are not path-prefix relative")
	}
}

func TestMutationRequiresDashboardHeader(t *testing.T) {
	t.Parallel()
	service := newTestServer(t)
	request := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(`{"label":"Test","path":"/tmp"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	service.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response status = %d, want 403", response.Code)
	}
	if response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("security headers are missing")
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	service, _ := newTestServerWithStore(t, inertRunner{})
	return service
}

func newTestServerWithStore(t *testing.T, runner claude.CommandRunner) (*Server, *store.Store) {
	t.Helper()
	root := t.TempDir()
	layout := paths.Layout{
		Root: root, Database: filepath.Join(root, "state.sqlite3"), Profiles: filepath.Join(root, "claude-profiles"),
		Logs: filepath.Join(root, "logs"), Binary: filepath.Join(root, "bin", "vibe-remote"),
		CodexHooks: filepath.Join(root, "hooks.json"), CodexHookVerified: filepath.Join(root, "verified"),
	}
	if err := os.MkdirAll(filepath.Dir(layout.Binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.Binary, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(layout.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	manager, err := claude.New(root, database, claude.Options{
		Runner: runner, ReadyTimeout: 2 * time.Second, StopTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	service, err := New(Options{Store: database, Claude: manager, Layout: layout, Binary: layout.Binary})
	if err != nil {
		t.Fatal(err)
	}
	return service, database
}

type inertRunner struct{}

func (inertRunner) RunInteractive(context.Context, claude.Command) error { return nil }
func (inertRunner) Output(context.Context, claude.Command) ([]byte, error) {
	return []byte(`{"loggedIn":false}`), nil
}
func (inertRunner) Start(claude.Command) (claude.Process, error) {
	return nil, context.Canceled
}

func TestUniqueSessionNameKeepsParallelSessionsDistinct(t *testing.T) {
	t.Parallel()
	workspace := model.Workspace{Label: "Admin panel", Path: "/Users/me/admin"}
	existing := []model.RemoteSession{
		{ID: "one", Name: "Vibe Remote · person@example.com · Admin panel"},
		{ID: "two", Name: "Vibe Remote · person@example.com · Admin panel (2)"},
	}
	got := uniqueSessionName("", "person@example.com", workspace, existing, "")
	if got != "Vibe Remote · person@example.com · Admin panel (3)" {
		t.Fatalf("generated name = %q", got)
	}
	if renamed := uniqueSessionName("", "person@example.com", workspace, existing, "one"); renamed != "Vibe Remote · person@example.com · Admin panel" {
		t.Fatalf("a session may reuse its own name: %q", renamed)
	}
	if custom := uniqueSessionName("  Nightly   fixes ", "person@example.com", workspace, existing, ""); custom != "Nightly fixes" {
		t.Fatalf("custom name = %q", custom)
	}
}

func TestResolveResumeRejectsForeignConversations(t *testing.T) {
	t.Parallel()
	service := newTestServer(t)
	ctx := context.Background()
	session, err := service.store.UpsertSession(ctx, model.Session{
		Provider: model.ProviderClaude, NativeSessionID: "native-1", AccountID: "account-a",
		WorkspacePath: "/Users/me/admin", State: model.SessionStopped,
	})
	if err != nil {
		t.Fatal(err)
	}

	resume, err := service.resolveResume(ctx, &session.ID, "", "account-a", "/Users/me/admin")
	if err != nil || resume != "native-1" {
		t.Fatalf("resolveResume() = %q, %v", resume, err)
	}
	if _, err := service.resolveResume(ctx, &session.ID, "", "account-b", "/Users/me/admin"); err == nil {
		t.Fatal("a conversation from another account was accepted")
	}
	if _, err := service.resolveResume(ctx, &session.ID, "", "account-a", "/Users/me/other"); err == nil {
		t.Fatal("a conversation from another workspace was accepted")
	}
	empty := ""
	if resume, err := service.resolveResume(ctx, &empty, "native-1", "account-a", "/Users/me/admin"); err != nil || resume != "" {
		t.Fatalf("an explicit empty request must start a new conversation: %q, %v", resume, err)
	}
}

// An omitted conversation keeps the link the checkpoint hook made, so editing a
// slot's name never detaches the conversation it is talking in.
func TestResolveResumeKeepsTheLinkedConversationWhenNoneIsRequested(t *testing.T) {
	t.Parallel()
	service := newTestServer(t)
	ctx := context.Background()

	resume, err := service.resolveResume(ctx, nil, "native-1", "account-a", "/Users/me/admin")
	if err != nil || resume != "native-1" {
		t.Fatalf("resolveResume() = %q, %v", resume, err)
	}
	if resume, err := service.resolveResume(ctx, nil, "", "account-a", "/Users/me/admin"); err != nil || resume != "" {
		t.Fatalf("an unlinked slot must stay unlinked: %q, %v", resume, err)
	}
	if _, err := service.resolveResume(ctx, nil, "--dangerously-skip-permissions", "account-a", "/Users/me/admin"); err == nil {
		t.Fatal("an unsafe stored conversation was accepted")
	}
}

// A cold Claude profile can take over a minute to register Remote Control, and
// activation is serialized across slots, so restoring on the startup path used
// to leave the dashboard unreachable for the length of the whole sweep.
func TestRestoreDoesNotBlockTheDashboard(t *testing.T) {
	t.Parallel()
	service, database := newTestServerWithStore(t, blockingRunner{})
	seedRunnableSlot(t, service, database, "rs_slow", "11111111-2222-3333-4444-555555555555")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The stub blocks until the manager's 2s ready timeout, standing in for a
	// cold profile that takes over a minute in production.
	started := time.Now()
	service.RestoreInBackground(ctx, func(error) {})
	if handed := time.Since(started); handed > time.Second {
		t.Fatalf("RestoreInBackground blocked for %s; the listener must start first", handed)
	}

	// The slot is still starting, so this exercises the window that used to be
	// dark rather than the state after the sweep finished.
	request := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	response := httptest.NewRecorder()
	served := make(chan int, 1)
	go func() {
		service.http.Handler.ServeHTTP(response, request)
		served <- response.Code
	}()
	select {
	case code := <-served:
		if code != http.StatusOK {
			t.Fatalf("GET /api/state during restore = %d, want 200", code)
		}
	case <-time.After(time.Second):
		t.Fatal("GET /api/state blocked while a slot was being restored")
	}
}

// The user may remove or stop a slot from the dashboard while an earlier slot
// is still starting, so each slot is re-read before it is activated.
func TestRestoreSkipsSlotsChangedMidSweep(t *testing.T) {
	t.Parallel()
	service, database := newTestServerWithStore(t, inertRunner{})
	seedRunnableSlot(t, service, database, "rs_gone", "66666666-7777-8888-9999-aaaaaaaaaaaa")
	if err := database.DeleteRemoteSession(context.Background(), "rs_gone"); err != nil {
		t.Fatal(err)
	}
	if err := service.restoreOne(context.Background(), "rs_gone"); err != "" {
		t.Fatalf("restoreOne on a removed slot = %q, want no failure", err)
	}
}

func seedRunnableSlot(t *testing.T, service *Server, database *store.Store, id, uuid string) {
	t.Helper()
	ctx := context.Background()
	// The manager derives the profile directory from the account ID and refuses
	// any stored path that does not match.
	profileDir := filepath.Join(service.layout.Profiles, uuid)
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The manager only accepts UUID account IDs, which is what it assigns when
	// an account is added; the store's own newID prefix form would be rejected.
	account, err := database.UpsertAccount(ctx, model.Account{
		ID: uuid, Email: id + "@example.com", ProfileDir: profileDir, Status: model.AccountAuthenticated,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := database.UpsertWorkspace(ctx, model.Workspace{Label: id, Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertRemoteSession(ctx, model.RemoteSession{
		ID: id, Name: id, AccountID: account.ID, WorkspaceID: workspace.ID,
		WorkspacePath: workspace.Path, Desired: model.DesiredRunning,
	}); err != nil {
		t.Fatal(err)
	}
}

// blockingRunner starts a process that never reports readiness, standing in for
// a cold profile that is still registering Remote Control.
type blockingRunner struct{ inertRunner }

func (blockingRunner) Output(context.Context, claude.Command) ([]byte, error) {
	return []byte(`{"loggedIn":true}`), nil
}

func (blockingRunner) Start(claude.Command) (claude.Process, error) {
	return &blockingProcess{ready: make(chan struct{}), done: make(chan struct{})}, nil
}

type blockingProcess struct {
	ready chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (*blockingProcess) PID() int                 { return 0 }
func (p *blockingProcess) Ready() <-chan struct{} { return p.ready }
func (*blockingProcess) ReadyError() error        { return nil }
func (*blockingProcess) RemoteURL() string        { return "" }
func (*blockingProcess) Signal(os.Signal) error   { return nil }
func (p *blockingProcess) Kill() error            { p.stop(); return nil }
func (p *blockingProcess) Wait() error            { <-p.done; return nil }
func (p *blockingProcess) stop()                  { p.once.Do(func() { close(p.done) }) }

// Tailscale Serve proxies to the same loopback listener the Mac's own browser
// uses, so the peer address alone cannot tell the phone from this Mac.
func TestLocalRequestIgnoresProxiedLoopback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		address string
		headers map[string]string
		want    bool
	}{
		{name: "this Mac", address: "127.0.0.1:53124", want: true},
		{name: "phone through Serve", address: "127.0.0.1:53125", headers: map[string]string{"X-Forwarded-For": "100.64.0.7"}, want: false},
		{name: "identified tailnet user", address: "127.0.0.1:53126", headers: map[string]string{"Tailscale-User-Login": "someone@example.com"}, want: false},
		{name: "another host", address: "100.64.0.7:53127", want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/state", nil)
			request.RemoteAddr = testCase.address
			for name, value := range testCase.headers {
				request.Header.Set(name, value)
			}
			if got := isLocalRequest(request); got != testCase.want {
				t.Fatalf("isLocalRequest() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// Only a link Claude itself printed may be handed to `open`.
func TestClaudeRemoteURLRejectsForeignLinks(t *testing.T) {
	t.Parallel()
	valid := []string{"https://claude.ai/code", "https://claude.ai/code/cse_test", "https://claude.com/code/cse_test"}
	for _, candidate := range valid {
		if !isClaudeRemoteURL(candidate) {
			t.Fatalf("isClaudeRemoteURL(%q) = false, want true", candidate)
		}
	}
	invalid := []string{"", "http://claude.ai/code", "https://claude.ai.evil.test/code", "https://claude.ai/settings", "file:///Applications", "https://claude.ai/codex"}
	for _, candidate := range invalid {
		if isClaudeRemoteURL(candidate) {
			t.Fatalf("isClaudeRemoteURL(%q) = true, want false", candidate)
		}
	}
}

func TestClaudeDesktopDeepLink(t *testing.T) {
	t.Parallel()
	cases := []struct {
		remote string
		want   string
	}{
		{remote: "https://claude.ai/code/cse_01AC1WzhhYzERyni8guwwcSD", want: "claude://claude.ai/code/cse_01AC1WzhhYzERyni8guwwcSD"},
		{remote: "https://claude.com/code/session_test-1", want: "claude://claude.ai/code/session_test-1"},
		{remote: "https://claude.ai/code"},
		{remote: "https://claude.ai/code/not-a-session"},
		{remote: "https://claude.ai/code/cse_test/extra"},
		{remote: "https://example.com/code/cse_test"},
	}
	for _, testCase := range cases {
		got, ok := claudeDesktopDeepLink(testCase.remote)
		if got != testCase.want || ok != (testCase.want != "") {
			t.Fatalf("claudeDesktopDeepLink(%q) = %q, %v, want %q, %v", testCase.remote, got, ok, testCase.want, testCase.want != "")
		}
	}
}

// Opening the app happens on the Mac, so a phone asking for it would launch a
// window nobody is looking at.
func TestOpenDesktopRefusedFromTailnet(t *testing.T) {
	t.Parallel()
	service, database := newTestServerWithStore(t, inertRunner{})
	seedRunnableSlot(t, service, database, "rs_desktop", "11111111-2222-3333-4444-555555555555")
	request := httptest.NewRequest(http.MethodPost, "/api/remote-sessions/rs_desktop/open-desktop", nil)
	request.RemoteAddr = "127.0.0.1:53200"
	request.Header.Set("X-Forwarded-For", "100.64.0.7")
	request.Header.Set("X-Vibe-Remote", "1")
	response := httptest.NewRecorder()
	service.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("POST open-desktop from the tailnet = %d, want 403", response.Code)
	}
}
