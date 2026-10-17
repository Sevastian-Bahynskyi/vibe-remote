package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitSharedSessionSettingsRefreshForEachLaunch(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "settings.json")
	shared := filepath.Join(dir, "settings.shared.json")
	destination := filepath.Join(dir, "session.json")
	if err := os.WriteFile(source, []byte(`{"permissions":{"deny":["Bash(rm *)"]},"env":{"SECRET":"private"},"hooks":{"Stop":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first", "second"} {
		data := []byte(`{"env":{"JAVA_HOME":"` + value + `"},"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"shared-guard"}]}]}}`)
		if err := os.WriteFile(shared, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := provisionPermissions(source, destination); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		var settings permissionSettings
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Fatal(err)
		}
		if settings.SkipDangerousModePermissionPrompt != true || !settings.EnableAllProjectMcpServers || settings.Env["JAVA_HOME"] != value || settings.Env["SECRET"] != "" || len(settings.Hooks["PreToolUse"]) != 1 || settings.Hooks["Stop"] != nil || len(settings.Permissions.Deny) != 1 {
			t.Fatal("shared session settings did not refresh or leaked unrelated global settings")
		}
	}
	before, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte(`{"hooks":{"PreToolUse":"invalid"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionPermissions(source, destination); err == nil {
		t.Fatal("malformed shared hooks were accepted")
	}
	after, err := os.ReadFile(destination)
	if err != nil || string(after) != string(before) {
		t.Fatal("malformed shared settings overwrote the last valid session settings")
	}
	if err := os.Remove(shared); err != nil {
		t.Fatal(err)
	}
	if err := provisionPermissions(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	var settings permissionSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Env != nil || settings.Hooks != nil {
		t.Fatal("removed shared settings were retained")
	}
}

func TestPermissionsIsolateRulesAndRefreshRevocations(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.json")
	destination := filepath.Join(dir, "shared.json")
	for _, test := range []struct{ source, want string }{
		{`{"permissions":{"allow":["Bash(npm test)","Bash(gh issue edit *)"],"ask":["Bash(git push *)"],"deny":["Bash(rm *)"],"defaultMode":"bypassPermissions"},"env":{"SECRET":"test-only"},"hooks":{"Stop":[]}}`, `{"permissions":{"allow":["Bash(npm test)","Bash(gh issue edit *)"],"ask":["Bash(git push *)"],"deny":["Bash(rm *)"]},"skipDangerousModePermissionPrompt":true,"enableAllProjectMcpServers":true}`},
		{`{"permissions":{"deny":["Bash(gh issue edit *)"]}}`, `{"permissions":{"allow":["Bash(gh issue edit *)"],"deny":["Bash(gh issue edit *)"]},"skipDangerousModePermissionPrompt":true,"enableAllProjectMcpServers":true}`},
	} {
		if err := os.WriteFile(source, []byte(test.source), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := provisionPermissions(source, destination); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(destination)
		if err != nil || string(data) != test.want {
			t.Fatal("shared rules lost restrictions, retained revoked approvals, or included unrelated settings")
		}
		info, err := os.Stat(destination)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("permissions file must be private")
		}
	}
	if err := os.WriteFile(source, []byte(`{"permissions":{"deny":[12]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(destination)
	if err := provisionPermissions(source, destination); err == nil {
		t.Fatal("malformed restrictions were accepted")
	}
	after, err := os.ReadFile(destination)
	if err != nil || string(after) != string(before) {
		t.Fatal("malformed restrictions overwrote existing rules")
	}
}

func TestOutputStylePreservesPermissionsAndHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"permissions":{"deny":["Bash(rm *)"]},"hooks":{"Stop":[]},"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionOutputStyle(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	want := `{"hooks":{"Stop":[]},"outputStyle":"Codex","permissions":{"deny":["Bash(rm *)"]},"theme":"dark"}`
	if err != nil || string(data) != want {
		t.Fatal("profile settings were lost during provisioning")
	}
}
