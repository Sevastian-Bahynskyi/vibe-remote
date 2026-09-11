package claude

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/checkpoint"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestContinuationStartupThenResumeDoesNotReplayPrompt(t *testing.T) {
	db := newFakeStore()
	runner := &fakeRunner{outputs: [][]byte{[]byte(`{"loggedIn":true}`), []byte(`{"loggedIn":true}`)},
		processes: []Process{newFakeProcess(101, true, true), newFakeProcess(102, true, true)}}
	m := newTestManager(t, db, runner)
	account := addStoredAccount(t, m, db)
	spec := WorkerSpec{ID: "slot-a", AccountID: account.ID, Workspace: t.TempDir(), Name: "Thinga"}
	root := filepath.Dir(m.profilesRoot)
	if err := checkpoint.SaveContinuation(root, spec.ID, "private context"); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	first := runner.startAt(t, 0)
	if first.Args[len(first.Args)-1] != checkpoint.ContinuationPrompt {
		t.Fatal("missing startup prompt")
	}
	if err := checkpoint.RemoveContinuation(root, spec.ID); err != nil {
		t.Fatal(err)
	}
	const resume = "2d758a1d-3630-4765-9f84-e91a8de810c7"
	db.mu.Lock()
	db.slots[spec.ID] = model.RemoteSession{ID: spec.ID, ResumeSessionID: resume}
	db.mu.Unlock()
	spec.Force = true
	if err := m.Activate(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(runner.startAt(t, 1).Args, " ")
	if strings.Contains(args, checkpoint.ContinuationPrompt) || !strings.Contains(args, "--resume "+resume) {
		t.Fatal("restart did not resume without repeating bootstrap")
	}
}
