package install

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectSigningIdentityUsesCertificateBackedApplicationIdentity(t *testing.T) {
	t.Parallel()
	output := `  1) AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA "Apple Development: Test"
  2) BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB "Apple Distribution: Test"
  3) CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC "Developer ID Application: Test"`
	if got := selectSigningIdentity(output); got != "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC" {
		t.Fatalf("selected identity = %q", got)
	}
	if got := selectSigningIdentity(`  1) AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA "Apple Development: Test"`); got != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("development identity = %q", got)
	}
	if got := selectSigningIdentity(`  1) BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB "Apple Distribution: Test"`); got != "" {
		t.Fatal("selected an unsupported signing identity")
	}
}

func TestSigningFailurePreservesInstalledExecutable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source, installed := filepath.Join(root, "source"), filepath.Join(root, "installed")
	for path, data := range map[string]string{source: "new executable", installed: "previous executable"} {
		if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	signingError := errors.New("signing identity unavailable")
	err := copyExecutable(source, installed, func(path string) error {
		if path != installed+".new" {
			t.Error("signing did not use the staged executable")
		}
		return signingError
	})
	if !errors.Is(err, signingError) {
		t.Fatalf("install error = %v", err)
	}
	data, err := os.ReadFile(installed)
	if err != nil || string(data) != "previous executable" {
		t.Fatal("signing failure replaced the installed executable")
	}
	if _, err := os.Stat(installed + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed staged executable was left behind")
	}
}
