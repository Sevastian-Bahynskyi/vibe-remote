package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestWorkerEnvironmentEnforcesIdentityAfterDirenv(t *testing.T) {
	fakeDirenv(t, `{"CLAUDE_CONFIG_DIR":"/wrong","VIBE_REMOTE_ACCOUNT_ID":"wrong","VIBE_REMOTE_SLOT_ID":"wrong","VIBE_REMOTE_ACTIVITY_RUN_ID":"wrong","ANTHROPIC_API_KEY":"fixture","CLAUDE_CODE_USE_BEDROCK":"1","PROJECT_MCP_TOKEN":"fixture"}`)
	m := newTestManager(t, newFakeStore(), &fakeRunner{})
	w := &worker{id: "slot-a", account: model.Account{ID: "account-a", ProfileDir: "/selected"}, workspace: t.TempDir(), activityRunID: "run-a"}
	env := m.workerEnv(w)
	assertEnv(t, env, "CLAUDE_CONFIG_DIR", "/selected")
	assertEnv(t, env, "VIBE_REMOTE_ACCOUNT_ID", "account-a")
	assertEnv(t, env, "VIBE_REMOTE_SLOT_ID", "slot-a")
	assertEnv(t, env, "VIBE_REMOTE_ACTIVITY_RUN_ID", "run-a")
	assertEnv(t, env, "PROJECT_MCP_TOKEN", "fixture")
	for _, value := range env {
		if strings.HasPrefix(value, "ANTHROPIC_API_KEY=") || strings.HasPrefix(value, "CLAUDE_CODE_USE_BEDROCK=") {
			t.Fatal("direnv crossed the authentication boundary")
		}
	}
}

func TestSharedSettingsRejectAuthenticationAndIdentityOverrides(t *testing.T) {
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_USE_VERTEX", "VIBE_REMOTE_ACTIVITY_RUN_ID"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "settings.shared.json"), []byte(`{"env":{"`+key+`":"fixture"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := provisionPermissions(filepath.Join(root, "settings.json"), filepath.Join(root, "effective.json")); err == nil {
				t.Fatal("protected shared override was accepted")
			}
		})
	}
}

func TestProfileAndProjectSettingsCannotReplaceAuthentication(t *testing.T) {
	for _, name := range []string{"settings.json", "settings.local.json"} {
		for _, value := range []string{`{"env":{"ANTHROPIC_AUTH_TOKEN":"fixture"}}`, `{"apiKeyHelper":"fixture-command"}`, `{"env":{"CLAUDE_CONFIG_DIR":"/other"}}`} {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
			if validateSettingsBoundary(directory) == nil {
				t.Fatal("authentication override accepted")
			}
		}
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "settings.json"), []byte(`{"env":{"PROJECT_MCP_TOKEN":"fixture"},"permissions":{"deny":["Bash(rm *)"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateSettingsBoundary(directory); err != nil {
		t.Fatal(err)
	}
}
