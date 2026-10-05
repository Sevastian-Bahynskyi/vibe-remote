package claude

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestMergeEnvOverridesDuplicateKeys(t *testing.T) {
	base := []string{"PATH=/bin", "FOO=old"}
	got := mergeEnv(base, []string{"FOO=new", "BAR=1"})
	want := []string{"PATH=/bin", "FOO=new", "BAR=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnv() = %#v, want %#v", got, want)
	}
}

func TestMergeEnvNoOverridesReturnsBaseUnchanged(t *testing.T) {
	base := []string{"PATH=/bin"}
	got := mergeEnv(base, nil)
	if len(got) != 1 || got[0] != "PATH=/bin" {
		t.Fatalf("mergeEnv() = %#v", got)
	}
}

// fakeDirenv puts a stub "direnv" binary on PATH that prints the given JSON,
// so direnvEnv's exec + parse path is exercised without depending on direnv
// actually being installed.
func fakeDirenv(t *testing.T, jsonOutput string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake direnv script assumes a POSIX shell")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + jsonOutput + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(bin, "direnv"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDirenvEnvReadsWorkspaceExportsAndDropsBookkeeping(t *testing.T) {
	fakeDirenv(t, `{"SUPABASE_TOKEN_ACCOUNT2":"sbp_test","DIRENV_DIFF":"ignored"}`)
	got := direnvEnv(t.TempDir())
	if len(got) != 1 || got[0] != "SUPABASE_TOKEN_ACCOUNT2=sbp_test" {
		t.Fatalf("direnvEnv() = %#v", got)
	}
}

func TestDirenvEnvMissingBinaryReturnsNil(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := direnvEnv(t.TempDir()); got != nil {
		t.Fatalf("direnvEnv() = %#v, want nil", got)
	}
}

func TestDirenvEnvNoExportsReturnsNil(t *testing.T) {
	fakeDirenv(t, "")
	if got := direnvEnv(t.TempDir()); got != nil {
		t.Fatalf("direnvEnv() = %#v, want nil", got)
	}
}
