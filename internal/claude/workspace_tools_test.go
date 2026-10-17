package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnableWorkspaceToolsCompletesFirstRunOnboarding(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		initial string
	}{
		{"fresh", ""},
		{"incomplete", `{"hasCompletedOnboarding":false,"hasCompletedClaudeInChromeOnboarding":false}`},
		{"configured", `{"hasCompletedOnboarding":true,"theme":"light","lastOnboardingVersion":"2.1.263","unrelated":{"keep":true}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := t.TempDir()
			path := filepath.Join(profile, ".claude.json")
			if test.initial != "" {
				if err := os.WriteFile(path, []byte(test.initial), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := enableWorkspaceTools(profile, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var state struct {
				HasCompletedOnboarding               bool
				HasCompletedClaudeInChromeOnboarding bool
				Theme                                string
				LastOnboardingVersion                string
				Unrelated                            struct{ Keep bool }
			}
			if err := json.Unmarshal(data, &state); err != nil {
				t.Fatal(err)
			}
			if !state.HasCompletedOnboarding || !state.HasCompletedClaudeInChromeOnboarding {
				t.Fatal("managed session would wait for first-run onboarding")
			}
			if test.name == "configured" && (state.Theme != "light" || state.LastOnboardingVersion != "2.1.263" || !state.Unrelated.Keep) {
				t.Fatal("existing preferences or unrelated Claude state changed")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("Claude profile state must stay private")
			}
		})
	}
}
