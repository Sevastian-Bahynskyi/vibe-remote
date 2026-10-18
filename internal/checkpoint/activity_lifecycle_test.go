package checkpoint

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestActivityFollowsConversationChangesWithinTheSameWorker(t *testing.T) {
	for _, startEvent := range []string{"SessionStart", "UserPromptSubmit"} {
		t.Run(startEvent, func(t *testing.T) {
			db := openCheckpointStore(t)
			defer db.Close()
			account, err := db.UpsertAccount(t.Context(), model.Account{Email: "test@example.com"})
			if err != nil {
				t.Fatal(err)
			}
			slot, err := db.UpsertRemoteSession(t.Context(), model.RemoteSession{AccountID: account.ID, WorkspacePath: "/work", ResumeSessionID: "first"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.BeginWorkerActivity(t.Context(), slot.ID, "run"); err != nil {
				t.Fatal(err)
			}
			service := NewService(db, staticGitCapturer{})
			origin := HookOrigin{AccountID: account.ID, SlotID: slot.ID, ActivityRunID: "run"}
			send := func(conversation, event, request string) {
				t.Helper()
				payload, err := json.Marshal(map[string]string{
					"hook_event_name": event, "session_id": conversation, "cwd": "/work",
					"tool_name": "mcp__computer-use__request_access", "tool_use_id": request,
					"prompt": "Continue the requested app task.",
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := service.HandleStdin(t.Context(), model.ProviderClaude, origin, strings.NewReader(string(payload))); err != nil {
					t.Fatal(err)
				}
			}
			checkTools := func(want int) {
				t.Helper()
				activity, err := db.WorkerActivity(t.Context(), slot.ID, origin.ActivityRunID)
				if err != nil || len(activity.Tools) != want {
					t.Fatalf("pending tool count = %d, want %d; error = %v", len(activity.Tools), want, err)
				}
			}
			send("first", "PreToolUse", "old-request")
			checkTools(1)
			send("first", "SessionEnd", "")
			send("first", "PreToolUse", "late-request")
			checkTools(0)
			send("second", startEvent, "")
			send("second", "PreToolUse", "new-request")
			checkTools(1)
			send("first", "PostToolUse", "new-request")
			send("first", "Stop", "")
			send("first", "SessionEnd", "")
			checkTools(1)
			remote, err := db.GetRemoteSession(t.Context(), slot.ID)
			if err != nil || remote.ResumeSessionID != "second" {
				t.Fatal("old conversation callback replaced the live conversation")
			}
			send("second", "PostToolUse", "new-request")
			checkTools(0)
			send("second", "PreToolUse", "left-behind")
			checkTools(1)
			send("third", startEvent, "")
			send("third", "PreToolUse", "new-request")
			checkTools(1)
			if err := db.EndWorkerActivity(t.Context(), slot.ID, origin.ActivityRunID); err != nil {
				t.Fatal(err)
			}
			send("fourth", "SessionStart", "")
			send("fourth", "UserPromptSubmit", "")
			send("fourth", "PreToolUse", "closed-worker-request")
			checkTools(0)
			remote, err = db.GetRemoteSession(t.Context(), slot.ID)
			if err != nil || remote.ResumeSessionID != "third" {
				t.Fatal("stopped worker changed the resume conversation")
			}
			if err := db.BeginWorkerActivity(t.Context(), slot.ID, "replacement"); err != nil {
				t.Fatal(err)
			}
			send("fourth", "SessionStart", "")
			send("fourth", "UserPromptSubmit", "")
			remote, err = db.GetRemoteSession(t.Context(), slot.ID)
			if err != nil || remote.ResumeSessionID != "third" {
				t.Fatal("replaced worker changed the resume conversation")
			}
		})
	}
}
