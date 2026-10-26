package checkpoint

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func TestManagedModelCallbacksAreScopedAndSeparateIntentFromObservation(t *testing.T) {
	db := openCheckpointStore(t)
	defer db.Close()
	ctx := context.Background()
	workspace := t.TempDir()
	account, err := db.UpsertAccount(ctx, model.Account{Email: "fixture@example.com", Status: model.AccountAuthenticated})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := db.UpsertRemoteSession(ctx, model.RemoteSession{AccountID: account.ID, WorkspacePath: workspace, ResumeSessionID: "current", Model: "opus", Effort: "high", RequestedModel: "opus", RequestedEffort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BeginWorkerActivity(ctx, slot.ID, "current-run"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordWorkerActivity(ctx, store.ActivityEvent{SlotID: slot.ID, RunID: "current-run", AccountID: account.ID, WorkspacePath: slot.WorkspacePath, SessionID: "current", Event: "SessionStart"}); err != nil {
		t.Fatal(err)
	}
	service := NewService(db, staticGitCapturer{})
	origin := HookOrigin{SlotID: slot.ID, AccountID: account.ID, ActivityRunID: "current-run"}
	for _, test := range []struct{ name, run, account, conversation, agent string }{
		{"old run", "old-run", account.ID, "current", ""},
		{"wrong account", "current-run", "other", "current", ""},
		{"old conversation", "current-run", account.ID, "old", ""},
		{"child agent", "current-run", account.ID, "current", "child"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"hook_event_name":"PostModelSwitch","session_id":%q,"cwd":%q,"agent_id":%q,"to_model":"sonnet","requested_model":"sonnet","source":"picker"}`, test.conversation, workspace, test.agent)
			_, err := service.HandleStdin(ctx, model.ProviderClaude, HookOrigin{SlotID: slot.ID, AccountID: test.account, ActivityRunID: test.run}, strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := db.GetRemoteSession(ctx, slot.ID)
			if got.Model != "opus" || got.RequestedModel != "opus" {
				t.Fatal("rejected callback changed current settings")
			}
		})
	}
	for _, source := range []string{"sdk", "auto"} {
		observed, requested := "sonnet[1m]", "sonnet[1m]"
		if source == "auto" {
			observed = "haiku"
			requested = ""
		}
		payload := fmt.Sprintf(`{"hook_event_name":"PostModelSwitch","session_id":"current","cwd":%q,"to_model":%q,"requested_model":%q,"source":%q,"effort":{"level":"high"}}`, workspace, observed, requested, source)
		if _, err := service.HandleStdin(ctx, model.ProviderClaude, origin, strings.NewReader(payload)); err != nil {
			t.Fatal(err)
		}
		got, _ := db.GetRemoteSession(ctx, slot.ID)
		if got.Model != observed || got.RequestedModel != "sonnet[1m]" || got.ObservedAt == nil {
			t.Fatalf("intent/observation=%+v", got)
		}
		settings, _ := db.GetAgentSettings(ctx, model.ProviderClaude, "current")
		if settings.RequestedModel != "sonnet[1m]" {
			t.Fatal("conversation intent was lost")
		}
	}
}
