package checkpoint

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestConfirmationHooksObserveWithoutApproving(t *testing.T) {
	database := openCheckpointStore(t)
	defer database.Close()
	ctx := context.Background()
	account, err := database.UpsertAccount(ctx, model.Account{Email: "test@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := database.UpsertRemoteSession(ctx, model.RemoteSession{AccountID: account.ID, WorkspacePath: "/work", ResumeSessionID: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.BeginWorkerActivity(ctx, slot.ID, "run"); err != nil {
		t.Fatal(err)
	}
	service := NewService(database, staticGitCapturer{})
	origin := HookOrigin{AccountID: account.ID, SlotID: slot.ID, ActivityRunID: "run"}
	payload := `{"hook_event_name":"Elicitation","session_id":"conversation","cwd":"/work","mcp_server_name":"supabase","elicitation_id":"request","message":"SECRET","requested_schema":{"secret":"SECRET"}}`
	response, err := service.HandleStdin(ctx, model.ProviderClaude, origin, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 0 {
		t.Fatal("observer must not answer a confirmation")
	}
	activity, err := database.WorkerActivity(ctx, slot.ID, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Confirmations) != 1 || activity.Confirmations[0].Name != "supabase" {
		t.Fatalf("confirmation missing: %+v", activity)
	}
	response, err = service.HandleStdin(ctx, model.ProviderClaude, origin, strings.NewReader(`{"hook_event_name":"ElicitationResult","session_id":"conversation","cwd":"/work","mcp_server_name":"supabase","elicitation_id":"request","action":"cancel","content":{"secret":"SECRET"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 0 {
		t.Fatal("observer must not override a response")
	}
	activity, err = database.WorkerActivity(ctx, slot.ID, "run")
	if err != nil || len(activity.Confirmations) != 0 {
		t.Fatalf("cancelled confirmation remains: %+v, %v", activity, err)
	}
	if _, err := service.HandleStdin(ctx, model.ProviderClaude, origin, strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	activity, err = database.WorkerActivity(ctx, slot.ID, "run")
	if err != nil || len(activity.Confirmations) != 0 {
		t.Fatal("late confirmation resurrected a resolved request")
	}
}

func TestToolHooksRemainIsolatedAndIgnoreLateCallbacks(t *testing.T) {
	database := openCheckpointStore(t)
	defer database.Close()
	ctx := context.Background()
	account, err := database.UpsertAccount(ctx, model.Account{Email: "test@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var slots []model.RemoteSession
	for _, conversation := range []string{"first", "second"} {
		slot, err := database.UpsertRemoteSession(ctx, model.RemoteSession{AccountID: account.ID, WorkspacePath: "/work", ResumeSessionID: conversation})
		if err != nil {
			t.Fatal(err)
		}
		if err := database.BeginWorkerActivity(ctx, slot.ID, conversation); err != nil {
			t.Fatal(err)
		}
		slots = append(slots, slot)
	}
	service := NewService(database, staticGitCapturer{})
	started := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return started }
	origin := HookOrigin{AccountID: account.ID, SlotID: slots[0].ID, ActivityRunID: "first"}
	send := func(event, id, session string) {
		t.Helper()
		payload := `{"hook_event_name":"` + event + `","session_id":"` + session + `","cwd":"/work","tool_name":"mcp__supabase__apply_migration","tool_use_id":"` + id + `","tool_input":{"query":"SECRET"},"tool_response":"SECRET","error":"SECRET"}`
		response, err := service.HandleStdin(ctx, model.ProviderClaude, origin, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Output) != 0 {
			t.Fatal("activity hook must not change execution")
		}
	}
	send("PreToolUse", "pending", "first")
	send("PreToolUse", "pending", "first")
	send("PreToolUse", "wrong-conversation", "second")
	activity, err := database.WorkerActivity(ctx, slots[0].ID, "first")
	if err != nil || len(activity.Tools) != 1 || !activity.Tools[0].StartedAt.Equal(started) {
		t.Fatalf("duplicate changed activity: %+v %v", activity, err)
	}
	other, err := database.WorkerActivity(ctx, slots[1].ID, "second")
	if err != nil || len(other.Tools) != 0 {
		t.Fatal("activity crossed slots")
	}
	send("PostToolUseFailure", "pending", "first")
	send("PreToolUse", "pending", "first")
	send("PostToolUse", "out-of-order", "first")
	send("PreToolUse", "out-of-order", "first")
	activity, err = database.WorkerActivity(ctx, slots[0].ID, "first")
	if err != nil || len(activity.Tools) != 0 {
		t.Fatal("completed tool was resurrected")
	}
	send("PreToolUse", "interrupted", "first")
	send("StopFailure", "", "first")
	send("PreToolUse", "late", "first")
	activity, err = database.WorkerActivity(ctx, slots[0].ID, "first")
	if err != nil || len(activity.Tools) != 0 {
		t.Fatal("stopped turn retained activity")
	}
	if err := database.BeginWorkerActivity(ctx, slots[0].ID, "replacement"); err != nil {
		t.Fatal(err)
	}
	send("PreToolUse", "old-process", "first")
	activity, err = database.WorkerActivity(ctx, slots[0].ID, "replacement")
	if err != nil || len(activity.Tools) != 0 {
		t.Fatal("old worker callback survived restart")
	}
}

func TestConfirmationWithoutRequestIDClearsOnToolFailureAndCanRepeat(t *testing.T) {
	database := openCheckpointStore(t)
	defer database.Close()
	ctx := context.Background()
	account, err := database.UpsertAccount(ctx, model.Account{Email: "test@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := database.UpsertRemoteSession(ctx, model.RemoteSession{AccountID: account.ID, WorkspacePath: "/work", ResumeSessionID: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.BeginWorkerActivity(ctx, slot.ID, "run"); err != nil {
		t.Fatal(err)
	}
	service := NewService(database, staticGitCapturer{})
	origin := HookOrigin{AccountID: account.ID, SlotID: slot.ID, ActivityRunID: "run"}
	send := func(payload string) {
		t.Helper()
		_, err := service.HandleStdin(ctx, model.ProviderClaude, origin, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"first", "second"} {
		send(`{"hook_event_name":"PreToolUse","session_id":"conversation","cwd":"/work","tool_name":"mcp__supabase__apply_migration","tool_use_id":"` + id + `"}`)
		send(`{"hook_event_name":"Elicitation","session_id":"conversation","cwd":"/work","mcp_server_name":"supabase"}`)
		activity, err := database.WorkerActivity(ctx, slot.ID, "run")
		if err != nil || len(activity.Confirmations) != 1 {
			t.Fatalf("request not observed: %+v, %v", activity, err)
		}
		send(`{"hook_event_name":"PostToolUseFailure","session_id":"conversation","cwd":"/work","tool_name":"mcp__supabase__apply_migration","tool_use_id":"` + id + `"}`)
		send(`{"hook_event_name":"Elicitation","session_id":"conversation","cwd":"/work","mcp_server_name":"supabase"}`)
		activity, err = database.WorkerActivity(ctx, slot.ID, "run")
		if err != nil || len(activity.Tools) != 0 || len(activity.Confirmations) != 0 {
			t.Fatalf("failed tool left a confirmation: %+v, %v", activity, err)
		}
	}
}

func TestConfirmationCorrelationSurvivesParallelAndOutOfOrderHooks(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmation before tool", true: "result after parallel completion"}[parallel], func(t *testing.T) {
			db := openCheckpointStore(t)
			defer db.Close()
			ctx := context.Background()
			account, err := db.UpsertAccount(ctx, model.Account{Email: "test@example.com"})
			if err != nil {
				t.Fatal(err)
			}
			slot, err := db.UpsertRemoteSession(ctx, model.RemoteSession{AccountID: account.ID, WorkspacePath: "/work", ResumeSessionID: "conversation"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.BeginWorkerActivity(ctx, slot.ID, "run"); err != nil {
				t.Fatal(err)
			}
			service := NewService(db, staticGitCapturer{})
			origin := HookOrigin{AccountID: account.ID, SlotID: slot.ID, ActivityRunID: "run"}
			send := func(event, id string) {
				t.Helper()
				payload := `{"hook_event_name":"` + event + `","session_id":"conversation","cwd":"/work","mcp_server_name":"supabase","tool_name":"mcp__supabase__apply_migration","tool_use_id":"` + id + `"}`
				if _, err := service.HandleStdin(ctx, model.ProviderClaude, origin, strings.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
			}
			if parallel {
				send("PreToolUse", "first")
				send("PreToolUse", "second")
			}
			send("Elicitation", "")
			if !parallel {
				send("PreToolUse", "first")
			}
			send("PostToolUseFailure", "first")
			if parallel {
				send("ElicitationResult", "")
				send("Elicitation", "")
			}
			activity, err := db.WorkerActivity(ctx, slot.ID, "run")
			if err != nil || len(activity.Confirmations) != 0 {
				t.Fatalf("resolved request remains: %+v, %v", activity, err)
			}
		})
	}
}

func TestNewConfirmationBeforeToolStartDoesNotInheritEarlierCompletion(t *testing.T) {
	db := openCheckpointStore(t)
	defer db.Close()
	ctx := context.Background()
	account, err := db.UpsertAccount(ctx, model.Account{Email: "test@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := db.UpsertRemoteSession(ctx, model.RemoteSession{AccountID: account.ID, WorkspacePath: "/work", ResumeSessionID: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BeginWorkerActivity(ctx, slot.ID, "run"); err != nil {
		t.Fatal(err)
	}
	service := NewService(db, staticGitCapturer{})
	origin := HookOrigin{AccountID: account.ID, SlotID: slot.ID, ActivityRunID: "run"}
	send := func(event, id string) {
		t.Helper()
		payload := `{"hook_event_name":"` + event + `","session_id":"conversation","cwd":"/work","mcp_server_name":"supabase","elicitation_id":"request","tool_name":"mcp__supabase__apply_migration","tool_use_id":"` + id + `"}`
		if _, err := service.HandleStdin(ctx, model.ProviderClaude, origin, strings.NewReader(payload)); err != nil {
			t.Fatal(err)
		}
	}
	send("PreToolUse", "old")
	send("PostToolUse", "old")
	send("Elicitation", "")
	send("PreToolUse", "new")
	activity, err := db.WorkerActivity(ctx, slot.ID, "run")
	if err != nil || len(activity.Confirmations) != 1 {
		t.Fatalf("new confirmation was suppressed: %+v, %v", activity, err)
	}
	send("PostToolUseFailure", "new")
	activity, err = db.WorkerActivity(ctx, slot.ID, "run")
	if err != nil || len(activity.Confirmations) != 0 {
		t.Fatal("failed tool retained its confirmation")
	}
}
