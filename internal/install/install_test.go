package install

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/paths"
	systemstate "github.com/Sevastian-Bahynskyi/vibe-remote/internal/system"
)

func TestInstallManagedFilesDoesNotVerifyNewCodexHooks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout := paths.Layout{
		Binary:            filepath.Join(root, "installed", "vibe-remote"),
		CodexHooks:        filepath.Join(root, ".codex", "hooks.json"),
		CodexConfig:       filepath.Join(root, ".codex", "config.toml"),
		InstallRecord:     filepath.Join(root, "install-record.json"),
		CodexHookVerified: filepath.Join(root, "codex-hook-verified"),
	}
	if err := os.MkdirAll(filepath.Dir(layout.Binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.CodexConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.CodexConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "new-vibe-remote")
	if err := os.WriteFile(source, []byte("new executable"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installManagedFiles(layout, source); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(layout.CodexHookVerified); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification marker exists before a real hook event: %v", err)
	}
}

func TestInstallManagedFilesPreservesVerifiedCodexHooks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout := paths.Layout{
		Binary:            filepath.Join(root, "installed", "vibe-remote"),
		CodexHooks:        filepath.Join(root, ".codex", "hooks.json"),
		CodexConfig:       filepath.Join(root, ".codex", "config.toml"),
		InstallRecord:     filepath.Join(root, "install-record.json"),
		CodexHookVerified: filepath.Join(root, "codex-hook-verified"),
	}
	if err := os.MkdirAll(filepath.Dir(layout.Binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.CodexHooks), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.Binary, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.CodexConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeCodexHooks(layout.CodexHooks, layout.Binary); err != nil {
		t.Fatal(err)
	}
	verified, err := systemstate.HookFingerprint(layout.CodexHooks, layout.Binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.CodexHookVerified, []byte(verified+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "new-vibe-remote")
	if err := os.WriteFile(source, []byte("new executable"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installManagedFiles(layout, source); err != nil {
		t.Fatal(err)
	}

	current, err := systemstate.HookFingerprint(layout.CodexHooks, layout.Binary)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(layout.CodexHookVerified)
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != current+"\n" {
		t.Fatalf("verification marker = %q, want %q", marker, current+"\n")
	}
}

func TestInstallClaudeHooksIsIdempotentAndRepairsOwnedHandler(t *testing.T) {
	t.Parallel()
	profile := t.TempDir()
	binary := "/Applications/Vibe Remote/bin/vibe-remote"
	settingsPath := filepath.Join(profile, "settings.json")
	legacyCommand := hookCommand(binary, "claude", "UserPromptSubmit")
	document := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"UserPromptSubmit": []any{
				map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": legacyCommand, "timeout": float64(5), "additionalContextLimit": float64(4500)},
					map[string]any{"type": "command", "command": "keep-me", "timeout": float64(1)},
				}},
			},
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallClaudeHooks(profile, binary); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallClaudeHooks(profile, binary); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("second installation changed the settings")
	}
	var updated map[string]any
	if err := json.Unmarshal(second, &updated); err != nil {
		t.Fatal(err)
	}
	if updated["theme"] != "dark" {
		t.Fatal("unrelated Claude setting was not preserved")
	}
	hooks := updated["hooks"].(map[string]any)
	groups := hooks["UserPromptSubmit"].([]any)
	handlers := groups[0].(map[string]any)["hooks"].([]any)
	if len(handlers) != 2 {
		t.Fatalf("handler count = %d, want 2", len(handlers))
	}
	owned := handlers[0].(map[string]any)
	if owned["timeout"] != float64(3) || owned["additionalContextLimit"] != float64(12000) {
		t.Fatalf("owned handler was not repaired: %#v", owned)
	}
}

func TestInstallClaudeHooksRejectsMalformedExistingHooks(t *testing.T) {
	t.Parallel()
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), []byte(`{"hooks":{"Stop":"invalid"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallClaudeHooks(profile, "/bin/vibe-remote"); err == nil {
		t.Fatal("malformed hook collection was silently replaced")
	}
}

func TestFunnelActive(t *testing.T) {
	t.Parallel()
	if funnelActive("https://host.example.ts.net (tailnet only)\n") {
		t.Fatal("tailnet-only Serve route was treated as Funnel")
	}
	if !funnelActive("Available on the internet:\nhttps://host.example.ts.net\n") {
		t.Fatal("internet-exposed Funnel route was not detected")
	}
}
