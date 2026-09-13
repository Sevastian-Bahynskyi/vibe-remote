package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// The service must survive Claude Code being absent. It used to exit during
// construction, which under launchd's KeepAlive became a respawn loop that took
// the dashboard down — including the health row naming the cause.
func TestNewStartsWithoutAClaudeBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	manager, err := New(t.TempDir(), newFakeStore(), Options{})
	if err != nil {
		t.Fatalf("New with no Claude Code on the system = %v; the service must still start", err)
	}
	if got := manager.BinaryPath(); got != "" {
		t.Errorf("BinaryPath() = %q, want empty so the health panel reports it missing", got)
	}
}

// ...and the operations that genuinely need the binary must say so plainly.
func TestActivateReportsAMissingClaudeBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	store := newFakeStore()
	manager, err := New(t.TempDir(), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	account := addStoredAccount(t, manager, store)
	err = manager.Activate(context.Background(), WorkerSpec{
		ID: "slot-a", AccountID: account.ID, Workspace: t.TempDir(), Name: "Project",
	})
	if err == nil {
		t.Fatal("Activate succeeded with no Claude Code installed")
	}
}

func TestFindBinaryPrefersPathThenFallsBackToClaudeDesktop(t *testing.T) {
	home := t.TempDir()
	pathDir := t.TempDir()
	t.Setenv("HOME", home)

	// Nothing anywhere.
	t.Setenv("PATH", pathDir)
	if _, err := FindBinary(); err == nil {
		t.Fatal("FindBinary found a binary that does not exist")
	}

	// Only Claude Desktop's bundled copies, of which the newest wins. 266 beats
	// 9 only if the comparison is numeric, which is the point of the two.
	bundle := func(version string) string {
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude-code",
			version, "claude.app", "Contents", "MacOS", "claude")
	}
	writeExecutable(t, bundle("2.1.9"))
	writeExecutable(t, bundle("2.1.266"))
	found, err := FindBinary()
	if err != nil {
		t.Fatalf("FindBinary with a bundled copy = %v", err)
	}
	if found != bundle("2.1.266") {
		t.Errorf("FindBinary() = %q, want the newest bundled copy %q", found, bundle("2.1.266"))
	}

	// A copy on PATH outranks the bundle.
	onPath := filepath.Join(pathDir, "claude")
	writeExecutable(t, onPath)
	found, err = FindBinary()
	if err != nil {
		t.Fatal(err)
	}
	if found != onPath {
		t.Errorf("FindBinary() = %q, want the PATH copy %q to win", found, onPath)
	}
}

// A reinstall must heal a running service without a restart, which is the whole
// reason resolution happens per call rather than once at construction.
func TestRunnerPicksUpAClaudeBinaryInstalledLater(t *testing.T) {
	pathDir := t.TempDir()
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", t.TempDir())

	runner := &execRunner{}
	if _, err := runner.binaryPath(); err == nil {
		t.Fatal("binaryPath resolved with nothing installed")
	}
	installed := filepath.Join(pathDir, "claude")
	writeExecutable(t, installed)
	got, err := runner.binaryPath()
	if err != nil {
		t.Fatalf("binaryPath after install = %v", err)
	}
	if got != installed {
		t.Errorf("binaryPath() = %q, want %q", got, installed)
	}
}

func TestCompareVersionsOrdersNumerically(t *testing.T) {
	t.Parallel()
	cases := []struct {
		left, right string
		want        int
	}{
		{"2.1.266", "2.1.9", 1},
		{"2.1.9", "2.1.266", -1},
		{"2.1.266", "2.1.266", 0},
		{"2.2", "2.1.999", 1},
		{"2.1", "2.1.0", 0},
	}
	for _, testCase := range cases {
		if got := compareVersions(testCase.left, testCase.right); got != testCase.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", testCase.left, testCase.right, got, testCase.want)
		}
	}
}
