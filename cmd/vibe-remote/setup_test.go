package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func TestSetupFailureIsSanitizedAndSuccessfulCompletionIsReady(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, fails := range []bool{true, false} {
		job, err := db.CreateSetupJob(ctx, "fixture@example.com", "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = runSetupJob(db, job.ID, func(context.Context) (model.Account, error) {
			if fails {
				return model.Account{}, errors.New("fixture sensitive OAuth output")
			}
			return model.Account{}, nil
		})
		if (err != nil) != fails {
			t.Fatalf("unexpected result %v", err)
		}
		got, _ := db.GetSetupJob(ctx, job.ID)
		want := model.SetupReady
		if fails {
			want = model.SetupFailed
		}
		if got.State != want || strings.Contains(got.Error, "OAuth") {
			t.Fatal("setup result was incorrect or exposed raw error")
		}
	}
}

func TestSetupCancellationCancelsTheOwnedAuthenticationOperation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	job, err := db.CreateSetupJob(context.Background(), "fixture@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := runSetupJob(db, job.ID, func(ctx context.Context) (model.Account, error) {
			close(started)
			<-ctx.Done()
			return model.Account{}, ctx.Err()
		})
		done <- err
	}()
	<-started
	if err := db.FinishSetupJob(context.Background(), job.ID, "", model.SetupCancelled); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not reach authentication")
	}
	got, _ := db.GetSetupJob(context.Background(), job.ID)
	if got.State != model.SetupCancelled {
		t.Fatal("cancellation was lost")
	}
}
