package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/checkpoint"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func TestDashboardShowsConfirmationAndToolTimingSeparately(t *testing.T) {
	runner := &continuationRunner{}
	s, db := newTestServerWithStore(t, runner)
	ctx := context.Background()
	seedRunnableSlot(t, s, db, "activity", "12345678-1234-4123-8123-123456789abc")
	slot, err := db.GetRemoteSession(ctx, "activity")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.claude.Activate(ctx, claude.WorkerSpec{ID: slot.ID, Name: slot.Name, AccountID: slot.AccountID, Workspace: slot.WorkspacePath}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for s.claude.Status(slot.ID).State != "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	runID := ""
	for _, env := range runner.commands[0].Env {
		if strings.HasPrefix(env, "VIBE_REMOTE_ACTIVITY_RUN_ID=") {
			runID = strings.TrimPrefix(env, "VIBE_REMOTE_ACTIVITY_RUN_ID=")
		}
	}
	if runID == "" {
		t.Fatal("worker identity missing")
	}
	if err := db.RecordWorkerActivity(ctx, store.ActivityEvent{SlotID: slot.ID, RunID: runID, SessionID: "conversation", AccountID: slot.AccountID, WorkspacePath: slot.WorkspacePath, Event: "PreToolUse", RequestID: "tool", Name: "mcp__supabase__apply_migration", OccurredAt: time.Now().Add(-3 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, s, "/ui/fragments/sessions", true)
	if !strings.Contains(body, "mcp__supabase__apply_migration") || !strings.Contains(body, "Taking longer") || strings.Contains(body, "Waiting for confirmation") {
		t.Fatalf("tool wait mislabeled: %s", body)
	}
	hooks := checkpoint.NewService(db, nil)
	payload, err := json.Marshal(map[string]string{"hook_event_name": "Elicitation", "session_id": "conversation", "cwd": slot.WorkspacePath, "mcp_server_name": "supabase", "elicitation_id": "request", "message": "SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := hooks.HandleStdin(ctx, model.ProviderClaude, checkpoint.HookOrigin{SlotID: slot.ID, AccountID: slot.AccountID, ActivityRunID: runID}, strings.NewReader(string(payload)))
	if err != nil || len(response.Output) != 0 {
		t.Fatalf("observer failed: %v", err)
	}
	_, body = get(t, s, "/ui/fragments/session/activity", true)
	if !strings.Contains(body, "Waiting for confirmation") || !strings.Contains(body, "supabase") || !strings.Contains(body, "data-refresh=") || strings.Contains(body, "SECRET") {
		t.Fatalf("confirmation not visible safely: %s", body)
	}
	_, progress := get(t, s, "/ui/fragments/activity/activity", true)
	if !strings.Contains(progress, "Waiting for confirmation") || !strings.Contains(progress, "Open this session in Claude") {
		t.Fatalf("confirmation guidance missing: %s", progress)
	}
	if err := s.claude.Deactivate(ctx, slot.ID, false); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, s, "/ui/fragments/sessions", true)
	if strings.Contains(body, "Waiting for confirmation") || strings.Contains(body, "mcp__supabase__apply_migration") {
		t.Fatal("stopped slot displays old activity")
	}
}
