package claude

import (
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeRetentionProtectsActiveCurrentPreviousAndRecent(t *testing.T) {
	m := newTestManager(t, newFakeStore(), &fakeRunner{})
	root := filepath.Join(filepath.Dir(m.profilesRoot), "claude-runtime")
	now := time.Now()
	ids := []string{}
	for _, letter := range []string{"a", "b", "c", "d", "e"} {
		id := strings.Repeat(letter, 64)
		ids = append(ids, id)
		directory := filepath.Join(root, id)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		age := now.Add(-8 * 24 * time.Hour)
		if letter == "d" {
			age = now
		}
		if err := os.Chtimes(directory, age, age); err != nil {
			t.Fatal(err)
		}
	}
	if err := writePrivateJSON(filepath.Join(filepath.Dir(m.profilesRoot), "runtime-history.json"), runtimeHistory{Current: ids[0], Previous: ids[1]}); err != nil {
		t.Fatal(err)
	}
	m.workers["active"] = &worker{process: &execProcess{cmd: &exec.Cmd{Path: filepath.Join(root, ids[2], "claude")}}, state: model.WorkerStatus{State: "running", Running: true}}
	if err := m.CleanupRuntimes(now); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		_, err := os.Stat(filepath.Join(root, id))
		if i == 4 {
			if !os.IsNotExist(err) {
				t.Fatal("unused old runtime retained")
			}
		} else if err != nil {
			t.Fatal("protected runtime removed", i, err)
		}
	}
}
