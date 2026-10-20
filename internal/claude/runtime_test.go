package claude

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestNativeRuntimeSnapshotSurvivesReplacingTheInstalledBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source := filepath.Join(dir, "installed-claude")
	runtimes := filepath.Join(dir, "runtimes")
	first := append([]byte{0xcf, 0xfa, 0xed, 0xfe}, []byte("signed-version-one")...)
	second := append([]byte{0xcf, 0xfa, 0xed, 0xfe}, []byte("signed-version-two")...)
	if err := os.WriteFile(source, first, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := snapshotNativeExecutable(source, runtimes)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := snapshotNativeExecutable(source, runtimes)
	if err != nil || reused != path {
		t.Fatal("unchanged executable did not reuse its snapshot")
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("reusing a runtime replaced its executable inode")
	}
	replacement := source + ".new"
	if err := os.WriteFile(replacement, second, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, source); err != nil {
		t.Fatal(err)
	}
	updated, err := snapshotNativeExecutable(source, runtimes)
	if err != nil || updated == path {
		t.Fatal("updated executable did not get a separate snapshot")
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, first) {
		t.Fatal("installed update modified the running session's executable")
	}
	if data, err := os.ReadFile(updated); err != nil || !bytes.Equal(data, second) {
		t.Fatal("new session would not use the updated executable")
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatal("installed update invalidated the running executable's inode")
	}
	if before.Mode().Perm() != 0o500 {
		t.Fatal("runtime snapshot must be executable and read-only")
	}
}

func TestRuntimeSnapshotLeavesScriptLaunchersInTheirPackage(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(source, []byte("#!/usr/bin/env node\nrequire('./cli.js');\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtimes := filepath.Join(t.TempDir(), "runtimes")
	path, err := snapshotNativeExecutable(source, runtimes)
	if err != nil || path != source {
		t.Fatal("script launcher was moved away from its dependencies")
	}
	if _, err := os.Stat(runtimes); !os.IsNotExist(err) {
		t.Fatal("script launcher created a native runtime snapshot")
	}
}

func TestConcurrentSessionsReuseTheSameNativeRuntime(t *testing.T) {
	t.Parallel()
	source := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(source, []byte("\xcf\xfa\xed\xfeversion"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtimes := t.TempDir()
	var group sync.WaitGroup
	results := make(chan string, 8)
	for i := 0; i < 8; i++ {
		group.Go(func() {
			path, err := snapshotNativeExecutable(source, runtimes)
			if err != nil {
				t.Error(err)
				return
			}
			results <- path
		})
	}
	group.Wait()
	close(results)
	want := ""
	for path := range results {
		if want != "" && path != want {
			t.Fatal("concurrent sessions used different runtime snapshots")
		}
		want = path
	}
}

func TestRuntimeSnapshotRejectsTamperingWithoutReplacingTheExecutable(t *testing.T) {
	t.Parallel()
	for _, symlink := range []bool{false, true} {
		source := filepath.Join(t.TempDir(), "claude")
		if err := os.WriteFile(source, []byte("\xcf\xfa\xed\xfeversion"), 0o700); err != nil {
			t.Fatal(err)
		}
		path, err := snapshotNativeExecutable(source, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if symlink {
			err = os.Symlink(source, path)
		} else {
			err = os.WriteFile(path, []byte("damaged"), 0o500)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := snapshotNativeExecutable(source, filepath.Dir(filepath.Dir(path))); err == nil {
			t.Fatal("modified runtime snapshot was accepted")
		}
	}
}
