package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestMetadataRetentionProtectsLiveConversationsAndCurrentActivity(t *testing.T) {
	now := time.Now().UTC()
	db := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return now })
	defer db.Close()
	ctx := context.Background()
	account, err := db.UpsertAccount(ctx, model.Account{ID: "account", Email: "fixture@example.com", ProfileDir: "/fixture", Status: model.AccountAuthenticated})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := db.UpsertRemoteSession(ctx, model.RemoteSession{ID: "slot", AccountID: account.ID, WorkspacePath: "/fixture", ResumeSessionID: "bound", Desired: model.DesiredRunning})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bound", "orphan", "recent"} {
		if err := db.RecordAgentSettings(ctx, model.ProviderClaude, id, AgentSettings{Model: "opus"}); err != nil {
			t.Fatal(err)
		}
	}
	old := formatTime(now.Add(-30 * 24 * time.Hour))
	if _, err := db.db.Exec(`UPDATE session_agents SET updated_at=? WHERE native_session_id<>'recent'`, old); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginWorkerActivity(ctx, slot.ID, "old-run"); err != nil {
		t.Fatal(err)
	}
	if err := db.EndWorkerActivity(ctx, slot.ID, "old-run"); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginWorkerActivity(ctx, slot.ID, "current-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO worker_activity_items(slot_id,kind,request_id,name,started_at) VALUES('slot','tool','current','fixture',?)`, formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if err := db.EndWorkerActivity(ctx, slot.ID, "old-run"); err != nil {
		t.Fatal(err)
	}
	if err := db.CleanupOrphanMetadata(ctx, now.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bound", "recent"} {
		if _, err := db.GetAgentSettings(ctx, model.ProviderClaude, id); err != nil {
			t.Fatal("protected agent removed", id, err)
		}
	}
	if _, err := db.GetAgentSettings(ctx, model.ProviderClaude, "orphan"); !errors.Is(err, ErrNotFound) {
		t.Fatal("orphan retained", err)
	}
	counters, err := db.Counters(ctx)
	if err != nil || counters.ActivityRows != 1 {
		t.Fatal("late completion removed current activity", counters, err)
	}
	if err := db.EndWorkerActivity(ctx, slot.ID, "current-run"); err != nil {
		t.Fatal(err)
	}
	counters, err = db.Counters(ctx)
	if err != nil || counters.ActivityRows != 0 {
		t.Fatal("closed activity retained", counters, err)
	}
}
