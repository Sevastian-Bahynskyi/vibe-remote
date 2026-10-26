package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type profilePluginFixture struct {
	fakeRunner
	source        string
	marketplaces  map[string][]marketplace
	plugins       map[string]map[string]bool
	changes       int
	brokenInstall bool
}

func (r *profilePluginFixture) Output(_ context.Context, command Command) ([]byte, error) {
	profile := ""
	for _, value := range command.Env {
		if strings.HasPrefix(value, "CLAUDE_CONFIG_DIR=") {
			profile = strings.TrimPrefix(value, "CLAUDE_CONFIG_DIR=")
		}
	}
	if r.plugins[profile] == nil {
		r.plugins[profile] = map[string]bool{}
	}
	args := command.Args
	if len(args) > 2 && args[1] == "marketplace" {
		switch args[2] {
		case "list":
			entries := r.marketplaces[profile]
			if entries == nil {
				entries = []marketplace{}
			}
			return json.Marshal(entries)
		case "add":
			r.changes++
			for _, entry := range r.marketplaces[r.source] {
				if entry.Repo == args[3] {
					r.marketplaces[profile] = append(r.marketplaces[profile], entry)
				}
			}
		case "remove":
			r.changes++
			entries := []marketplace{}
			for _, entry := range r.marketplaces[profile] {
				if entry.Name != args[3] {
					entries = append(entries, entry)
				}
			}
			r.marketplaces[profile] = entries
		}
	} else {
		switch args[1] {
		case "list":
			entries := []installedPlugin{}
			for id, enabled := range r.plugins[profile] {
				entries = append(entries, installedPlugin{ID: id, Enabled: enabled, Scope: "user"})
			}
			return json.Marshal(entries)
		case "install", "enable":
			r.changes++
			if !r.brokenInstall {
				r.plugins[profile][args[2]] = true
			}
		case "uninstall":
			r.changes++
			delete(r.plugins[profile], args[2])
		}
	}
	return []byte(`{}`), nil
}

func profileFixture(t *testing.T) (string, string, *profilePluginFixture) {
	t.Helper()
	source, profile := t.TempDir(), t.TempDir()
	for path, value := range map[string]string{"CLAUDE.md": "shared instructions", "commands/fixture.md": "shared command", "skills/fixture/SKILL.md": "shared skill", "agents/fixture.md": "shared agent", "settings.json": `{"theme":"dark","env":{"PRIVATE_GLOBAL":"do not copy"},"enabledPlugins":{"fixture@market":true}}`} {
		if err := privateWrite(filepath.Join(source, path), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &profilePluginFixture{source: source, marketplaces: map[string][]marketplace{source: {{Name: "market", Source: "github", Repo: "fixture/market"}}}, plugins: map[string]map[string]bool{}}
	return source, profile, runner
}

func TestFreshProfileBaselineIsCompleteAndIdempotent(t *testing.T) {
	source, profile, runner := profileFixture(t)
	if err := privateWrite(filepath.Join(profile, "settings.json"), []byte(`{"profileOnly":true,"permissions":{"deny":["Bash(rm *)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := reconcileBaseline(context.Background(), source, profile, runner)
	if err != nil || !first.Ready {
		t.Fatalf("readiness=%+v error=%v", first, err)
	}
	before, _ := os.Stat(filepath.Join(profile, "CLAUDE.md"))
	settingsBefore, _ := os.ReadFile(filepath.Join(profile, "settings.json"))
	changes := runner.changes
	second, err := reconcileBaseline(context.Background(), source, profile, runner)
	if err != nil || first.Fingerprint != second.Fingerprint {
		t.Fatal("baseline was not repeatable", err)
	}
	after, _ := os.Stat(filepath.Join(profile, "CLAUDE.md"))
	if !before.ModTime().Equal(after.ModTime()) || runner.changes != changes {
		t.Fatal("repeat reconciliation changed installed entries")
	}
	if strings.Contains(string(settingsBefore), "PRIVATE_GLOBAL") || !strings.Contains(string(settingsBefore), "profileOnly") || !strings.Contains(string(settingsBefore), "Bash(rm *)") {
		t.Fatal("baseline leaked global env or removed local restrictions")
	}
	for _, path := range []string{"CLAUDE.md", "skills/fixture/SKILL.md", "commands/fixture.md", "agents/fixture.md"} {
		if _, err := os.Stat(filepath.Join(profile, path)); err != nil {
			t.Fatal(path, err)
		}
	}
}

func TestBaselineConflictsRevocationsAndLocalAdditions(t *testing.T) {
	source, profile, runner := profileFixture(t)
	if err := privateWrite(filepath.Join(profile, "CLAUDE.md"), []byte("original profile instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := privateWrite(filepath.Join(profile, "skills/local/SKILL.md"), []byte("local addition"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.plugins[profile] = map[string]bool{"local@market": true}
	first, err := reconcileBaseline(context.Background(), source, profile, runner)
	if err != nil || len(first.Conflicts) == 0 {
		t.Fatal("conflict was not backed up and reported", err)
	}
	for _, path := range []string{"CLAUDE.md", "commands/fixture.md"} {
		if err := os.Remove(filepath.Join(source, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := privateWrite(filepath.Join(source, "settings.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileBaseline(context.Background(), source, profile, runner); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(profile, "CLAUDE.md"))
	if string(original) != "original profile instructions" {
		t.Fatal("revocation did not restore preserved local content")
	}
	if _, err := os.Stat(filepath.Join(profile, "commands/fixture.md")); !os.IsNotExist(err) {
		t.Fatal("owned revoked command remained")
	}
	if _, err := os.Stat(filepath.Join(profile, "skills/local/SKILL.md")); err != nil {
		t.Fatal("profile-only skill was removed")
	}
	if runner.plugins[profile]["fixture@market"] || !runner.plugins[profile]["local@market"] {
		t.Fatal("plugin revocation changed profile-only additions")
	}
	if len(runner.marketplaces[profile]) == 0 {
		t.Fatal("marketplace still used by a local plugin was revoked")
	}
}

func TestMissingRequiredPluginBlocksReadiness(t *testing.T) {
	source, profile, runner := profileFixture(t)
	runner.brokenInstall = true
	report, err := reconcileBaseline(context.Background(), source, profile, runner)
	if err == nil || report.Ready {
		t.Fatal("missing required plugin was reported ready")
	}
	stored := readProfileReadiness(profile)
	if stored.Ready || len(stored.Missing) == 0 {
		t.Fatal("failure readiness report was not saved")
	}
}
