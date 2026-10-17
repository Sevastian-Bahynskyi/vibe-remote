package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testSupabaseServer() supabaseServer {
	return supabaseServer{Type: "http", URL: "https://mcp.supabase.com/mcp?project_ref=abcdefghijklmnopqrst", Headers: map[string]string{"Authorization": "Bearer ${SUPABASE_TOKEN}"}}
}

func TestWorkspaceSupabaseFreshCheckoutUsesRepositoryMapping(t *testing.T) {
	canonical := t.TempDir()
	path := filepath.Join(t.TempDir(), "settings.json")
	settings := workspaceSupabaseSettings{
		Workspaces:   map[string]map[string]supabaseServer{canonical: {"supabase": testSupabaseServer()}},
		Repositories: map[string]string{"github.com/couplegoai/couplegoai": canonical},
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"git@github.com:CoupleGoAI/couplegoai.git", "https://github.com/CoupleGoAI/couplegoai.git", "ssh://git@github.com/CoupleGoAI/couplegoai.git", "https://github.com/other/couplegoai.git"} {
		workspace := t.TempDir()
		for _, args := range [][]string{{"init", workspace}, {"-C", workspace, "remote", "add", "origin", remote}} {
			if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git: %v %s", err, output)
			}
		}
		servers, credentials, err := resolveWorkspaceSupabase(path, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(remote, "/other/") {
			if len(servers) != 0 || credentials != "" {
				t.Fatal("unrelated repository inherited Supabase access")
			}
		} else if len(servers) != 1 || credentials != canonical {
			t.Fatal("fresh checkout did not inherit its configured connections and credential source")
		}
	}
}

func TestSupabaseCredentialInheritanceIsLimitedAndPreservesWorkspaceOverride(t *testing.T) {
	servers := map[string]supabaseServer{"supabase": testSupabaseServer()}
	canonical := []string{"SUPABASE_TOKEN=test-only", "UNRELATED_SECRET=never-copy", "APP_ENV=production"}
	for _, environment := range [][]string{{"APP_ENV=development"}, {"APP_ENV=development", "SUPABASE_TOKEN=workspace-token"}} {
		inherited := inheritSupabaseCredentials(servers, environment, canonical)
		values := map[string]string{}
		for _, entry := range inherited {
			name, value, _ := strings.Cut(entry, "=")
			values[name] = value
		}
		if values["UNRELATED_SECRET"] != "" || values["APP_ENV"] != "development" || values["SUPABASE_TOKEN"] == "" {
			t.Fatal("credential inheritance leaked unrelated environment or lost the workspace environment")
		}
		if len(environment) == 2 && values["SUPABASE_TOKEN"] != "workspace-token" {
			t.Fatal("canonical credentials replaced an explicit workspace credential")
		}
	}
}

func TestWorkspaceSupabaseValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*supabaseServer)
		invalid bool
	}{
		{"development", func(_ *supabaseServer) {}, false},
		{"production", func(s *supabaseServer) { s.URL += "&read_only=true" }, false},
		{"production-writable", func(_ *supabaseServer) {}, true},
		{"secret", func(s *supabaseServer) { s.Headers["Authorization"] = "Bearer secret" }, true},
		{"unscoped", func(s *supabaseServer) { s.URL = "https://mcp.supabase.com/mcp" }, true},
		{"untrusted", func(s *supabaseServer) { s.URL = strings.Replace(s.URL, "mcp.supabase.com", "example.com", 1) }, true},
		{"unsafe-query", func(s *supabaseServer) { s.URL += "&token=secret" }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := testSupabaseServer()
			test.mutate(&server)
			name := "supabase"
			if strings.HasPrefix(test.name, "production") {
				name = "supabase-production"
			}
			workspace := t.TempDir()
			path := filepath.Join(t.TempDir(), "settings.json")
			data, err := json.Marshal(workspaceSupabaseSettings{Workspaces: map[string]map[string]supabaseServer{workspace: {name: server}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = loadWorkspaceSupabase(path, workspace)
			if (err != nil) != test.invalid {
				t.Fatalf("error = %v, invalid = %v", err, test.invalid)
			}
		})
	}
}

func TestWorkspaceSupabaseProvisionPreservesOtherState(t *testing.T) {
	profile := t.TempDir()
	path := filepath.Join(profile, ".claude.json")
	original := `{"other":true,"projects":{"/repo":{"hasTrustDialogAccepted":true,"mcpServers":{"unrelated":{"command":"keep"}}},"/other":{"keep":true}}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	servers := map[string]supabaseServer{"supabase": testSupabaseServer()}
	if err := provisionWorkspaceSupabase(profile, "/repo", servers); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Other    bool
		Projects map[string]struct {
			Keep                   bool
			HasTrustDialogAccepted bool
			McpServers             map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if !document.Other || !document.Projects["/other"].Keep || !document.Projects["/repo"].HasTrustDialogAccepted || document.Projects["/repo"].McpServers["unrelated"] == nil || document.Projects["/repo"].McpServers["supabase"] == nil {
		t.Fatal("provisioning lost unrelated state or the managed connection")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("profile state must remain private")
	}
	permissions := filepath.Join(profile, "permissions.json")
	source := filepath.Join(profile, "source.json")
	if err := os.WriteFile(source, []byte(`{"permissions":{"deny":["mcp__supabase__execute_sql"],"ask":["mcp__supabase__list_tables"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionPermissions(source, permissions, supabasePermissionRules(servers)...); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(permissions)
	if err != nil {
		t.Fatal(err)
	}
	var settings permissionSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Permissions.Deny) != 1 || len(settings.Permissions.Ask) != 1 || settings.Permissions.Allow[0] != "mcp__supabase__*" {
		t.Fatal("managed allow rule lost explicit restrictions")
	}
}

func TestSupabaseAvailability(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		invalid bool
	}{
		{"available", 200, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"execute_sql"}]}}`, false},
		{"revoked", 401, `{"secret":"must not appear in error"}`, true},
		{"missing-sql", 200, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`, true},
		{"protocol-error", 200, `{"jsonrpc":"2.0","id":1,"error":{"message":"secret"}}`, true},
		{"malformed", 200, `not json`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-only" {
					t.Error("missing authentication")
				}
				var payload struct {
					Method string `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if test.status == http.StatusOK && payload.Method == "initialize" {
					w.Header().Set("Mcp-Session-Id", "test-session")
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`))
					return
				}
				if test.status == http.StatusOK {
					if r.Header.Get("Mcp-Session-Id") != "test-session" || r.Header.Get("MCP-Protocol-Version") != "2025-03-26" {
						t.Error("MCP handshake headers were not preserved")
					}
					if payload.Method == "notifications/initialized" {
						w.WriteHeader(http.StatusAccepted)
						return
					}
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			err := checkSupabaseEndpoint(context.Background(), server.URL, "Bearer test-only")
			if (err != nil) != test.invalid {
				t.Fatalf("error = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("remote response leaked into error")
			}
		})
	}
	if err := checkWorkspaceSupabase(context.Background(), map[string]supabaseServer{"supabase": testSupabaseServer()}, nil); err == nil {
		t.Fatal("missing token was accepted")
	}
}

func TestWorkspaceSupabaseLinkedWorktree(t *testing.T) {
	repo := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "linked")
	for _, args := range [][]string{{"init", repo}, {"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "initial"}, {"-C", repo, "worktree", "add", "--detach", worktree}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, output)
		}
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	data, err := json.Marshal(workspaceSupabaseSettings{Workspaces: map[string]map[string]supabaseServer{repo: {"supabase": testSupabaseServer()}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	servers, err := loadWorkspaceSupabase(path, worktree)
	if err != nil || len(servers) != 1 {
		t.Fatalf("worktree did not inherit repository connections: %v", err)
	}
}

func TestManagedSupabaseMissingCredentialStopsWorker(t *testing.T) {
	store := newFakeStore()
	runner := &fakeRunner{outputs: [][]byte{[]byte(`{"loggedIn":true,"email":"person@example.com"}`)}}
	manager := newTestManager(t, store, runner)
	account := addStoredAccount(t, manager, store)
	workspace := t.TempDir()
	server := testSupabaseServer()
	server.Headers["Authorization"] = "Bearer ${VIBE_REMOTE_TEST_MISSING_TOKEN}"
	t.Setenv("VIBE_REMOTE_TEST_MISSING_TOKEN", "")
	settings := workspaceSupabaseSettings{Workspaces: map[string]map[string]supabaseServer{workspace: {"supabase": server}}}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(manager.profilesRoot), "workspace-supabase.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = manager.Activate(context.Background(), WorkerSpec{ID: "slot-a", AccountID: account.ID, Workspace: workspace, Name: "Project"})
	if err == nil || !strings.Contains(err.Error(), "credential is missing") {
		t.Fatalf("missing credential did not stop startup: %v", err)
	}
	if manager.Status("slot-a").Running {
		t.Fatal("worker started without a Supabase credential")
	}
}

func TestWorkspaceSupabaseLive(t *testing.T) {
	path := os.Getenv("VIBE_REMOTE_TEST_SUPABASE_CONFIG")
	workspace := os.Getenv("VIBE_REMOTE_TEST_SUPABASE_WORKSPACE")
	if path == "" || workspace == "" {
		t.Skip("live check requires an explicit config and workspace")
	}
	servers, err := loadWorkspaceSupabase(path, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) == 0 {
		t.Fatal("live check found no configured servers")
	}
	if err := checkWorkspaceSupabase(context.Background(), servers, mergeEnv(os.Environ(), direnvEnv(workspace))); err != nil {
		t.Fatal(err)
	}
}
