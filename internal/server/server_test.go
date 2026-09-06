package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
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
	manager, err := claude.New(root, database, claude.Options{Runner: inertRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	service, err := New(Options{Store: database, Claude: manager, Layout: layout, Binary: layout.Binary})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type inertRunner struct{}

func (inertRunner) RunInteractive(context.Context, claude.Command) error { return nil }
func (inertRunner) Output(context.Context, claude.Command) ([]byte, error) {
	return []byte(`{"loggedIn":false}`), nil
}
func (inertRunner) Start(claude.Command) (claude.Process, error) {
	return nil, context.Canceled
}
