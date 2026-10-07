package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPermissionsIsolateRulesAndRefreshRevocations(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.json")
	destination := filepath.Join(dir, "shared.json")
	for _, test := range []struct{ source, want string }{
		{`{"permissions":{"allow":["Bash(npm test)","Bash(gh issue edit *)"],"ask":["Bash(git push *)"],"deny":["Bash(rm *)"],"defaultMode":"bypassPermissions"},"env":{"SECRET":"test-only"},"hooks":{"Stop":[]}}`, `{"permissions":{"allow":["Bash(npm test)","Bash(gh issue edit *)"],"ask":["Bash(git push *)"],"deny":["Bash(rm *)"]}}`},
		{`{"permissions":{"deny":["Bash(gh issue edit *)"]}}`, `{"permissions":{"allow":["Bash(gh issue edit *)"],"deny":["Bash(gh issue edit *)"]}}`},
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
