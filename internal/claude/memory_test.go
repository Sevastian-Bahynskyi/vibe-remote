package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedMemoryDeduplicatesPreservesConflictsAndUnifiesWorktrees(t *testing.T) {
	ctx := context.Background()
	root, normal, repo := t.TempDir(), t.TempDir(), t.TempDir()
	worktree := filepath.Join(t.TempDir(), "linked")
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git fixture failed: %s %v", output, err)
		}
	}
	git("init", "-q")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "--allow-empty", "-qm", "fixture")
	git("worktree", "add", "-qb", "fixture-linked", worktree)
	_, aliases, err := canonicalProject(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	mainPath := repo
	for _, alias := range aliases {
		if alias == repo {
			mainPath = alias
			break
		}
	}
	profiles := []string{normal, filepath.Join(root, "claude-profiles", "profile-a"), filepath.Join(root, "claude-profiles", "profile-b")}
	originals := map[string]string{}
	for i, profile := range profiles {
		value := "same learned note"
		if i == 2 {
			value = "different learned note"
		}
		path := filepath.Join(profile, "projects", claudeProjectKey(mainPath), "memory", "feedback.md")
		if err := privateWrite(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		originals[path] = value
	}
	linked := filepath.Join(profiles[2], "projects", claudeProjectKey(worktree), "memory", "linked.md")
	if err := privateWrite(linked, []byte("worktree-only note"), 0o600); err != nil {
		t.Fatal(err)
	}
	originals[linked] = "worktree-only note"
	first, err := PrepareProjectMemory(ctx, root, normal, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Imports) != 4 || first.Conflicts != 1 {
		t.Fatalf("imports=%+v", first)
	}
	targets := map[string]bool{}
	for _, entry := range first.Imports {
		targets[entry.Target] = true
	}
	if len(targets) != 3 {
		t.Fatal("identical content was not deduplicated")
	}
	if err := privateWrite(filepath.Join(first.Directory, "MEMORY.md"), []byte("Owner's current shared memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := PrepareProjectMemory(ctx, root, normal, worktree)
	if err != nil {
		t.Fatal(err)
	}
	if second.Directory != first.Directory || len(second.Imports) != 4 {
		t.Fatal("linked worktree duplicated imports or used another memory directory")
	}
	for path, value := range originals {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != value {
			t.Fatal("original memory was changed")
		}
	}
	index, _ := os.ReadFile(filepath.Join(second.Directory, "MEMORY.md"))
	if !strings.Contains(string(index), "Owner's current") || strings.Count(string(index), "](IMPORTS.md)") != 1 {
		t.Fatal("shared current memory was overwritten")
	}
	imports, _ := os.ReadFile(filepath.Join(second.Directory, "IMPORTS.md"))
	if !strings.Contains(string(imports), "DIFFERING VERSIONS") || !strings.Contains(string(imports), "Source:") {
		t.Fatal("conflicts or provenance are not visible")
	}
}

func TestNonGitProjectMemoryUsesCanonicalWorkspace(t *testing.T) {
	root, workspace := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	a, err := PrepareProjectMemory(context.Background(), root, "", workspace)
	if err != nil {
		t.Fatal(err)
	}
	b, err := PrepareProjectMemory(context.Background(), root, "", alias)
	if err != nil {
		t.Fatal(err)
	}
	if a.Directory != b.Directory {
		t.Fatal("workspace symlink created another project identity")
	}
}
