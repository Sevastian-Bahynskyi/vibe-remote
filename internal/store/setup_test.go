package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestSetupJobsAreDurableAndTerminalStatesCannotBeOverwritten(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	job, err := db.CreateSetupJob(ctx, "fixture@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSetupJob(ctx, "fixture@example.com", ""); err == nil {
		t.Fatal("duplicate pending login accepted")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.GetSetupJob(ctx, job.ID)
	if err != nil || got.State != model.SetupPending {
		t.Fatal("pending job did not survive reopen")
	}
	if err := db.FinishSetupJob(ctx, job.ID, "", model.SetupCancelled); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSetupJob(ctx, job.ID, "", model.SetupReady); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetSetupJob(ctx, job.ID)
	if got.State != model.SetupCancelled {
		t.Fatal("late completion overwrote cancellation")
	}
	if _, err := db.CreateSetupJob(ctx, "fixture@example.com", ""); err != nil {
		t.Fatal("cancelled setup could not retry")
	}
}

func TestSetupHeartbeatDistinguishesWaitingFromInterrupted(t *testing.T) {
	now := time.Now().UTC()
	db := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return now })
	defer db.Close()
	ctx := context.Background()
	waiting, _ := db.CreateSetupJob(ctx, "waiting@example.com", "")
	lost, _ := db.CreateSetupJob(ctx, "lost@example.com", "")
	now = now.Add(31 * time.Second)
	if err := db.TouchSetupJob(ctx, waiting.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.InterruptStaleSetupJobs(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetSetupJob(ctx, waiting.ID)
	if got.State != model.SetupPending {
		t.Fatal("healthy waiting login was interrupted")
	}
	got, _ = db.GetSetupJob(ctx, lost.ID)
	if got.State != model.SetupInterrupted {
		t.Fatal("lost helper was not marked interrupted")
	}
}
