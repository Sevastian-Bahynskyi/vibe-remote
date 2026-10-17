package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/checkpoint"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func TestSessionHandoffCarriesItsConversationToBothDestinations(t *testing.T) {
	for _, provider := range []model.Provider{model.ProviderClaude, model.ProviderCodex} {
		t.Run(string(provider), func(t *testing.T) {
			runner := &continuationRunner{}
			s, db := newTestServerWithStore(t, runner)
			ctx := context.Background()
			const sourceAccount = "12345678-1234-4123-8123-123456789abc"
			const destinationAccount = "22345678-1234-4123-8123-123456789abc"
			seedRunnableSlot(t, s, db, "handoff", sourceAccount)
			remote, _ := db.GetRemoteSession(ctx, "handoff")
			_, err := db.UpsertAccount(ctx, model.Account{ID: destinationAccount, Email: "destination@example.com", Status: model.AccountAuthenticated, ProfileDir: filepath.Join(s.layout.Profiles, destinationAccount)})
			if err != nil {
				t.Fatal(err)
			}
			source, _, err := db.RecordHookEvent(ctx, store.HookEvent{Provider: model.ProviderClaude, AccountID: sourceAccount, SlotID: remote.ID, NativeSessionID: "12345678-1234-4123-8123-123456789abc", WorkspacePath: remote.WorkspacePath, Kind: store.HookEventPrompt, Prompt: "Finish the current feature"})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.LinkRemoteSessionConversation(ctx, remote.ID, source.NativeSessionID); err != nil {
				t.Fatal(err)
			}
			_, body := get(t, s, "/?screen=session&id="+remote.ID, false)
			if !strings.Contains(body, "Hand off to Codex") || !strings.Contains(body, "Hand off to Claude") || strings.Contains(body, "Change account, workspace or conversation") {
				t.Fatal("direct handoff choices missing")
			}
			account := ""
			if provider == model.ProviderClaude {
				account = destinationAccount
			}
			status, body := post(t, s, "/ui/sessions/"+remote.ID+"/handoff", map[string]string{"destinationProvider": string(provider), "destinationAccountId": account})
			if status != http.StatusOK {
				t.Fatalf("handoff = %d: %s", status, body)
			}
			stopped, _ := db.GetRemoteSession(ctx, remote.ID)
			if stopped.Desired != model.DesiredStopped {
				t.Fatal("source was not stopped")
			}
			if provider == model.ProviderCodex {
				handoffs, err := db.ListHandoffs(ctx, false)
				if err != nil || len(handoffs) != 1 || handoffs[0].SourceSessionID != source.ID || handoffs[0].WorkspacePath != remote.WorkspacePath {
					t.Fatal("wrong Codex continuation", err)
				}
			} else {
				remotes, _ := db.ListRemoteSessions(ctx)
				for _, destination := range remotes {
					if destination.AccountID != destinationAccount {
						continue
					}
					text, err := checkpoint.LoadContinuation(s.layout.Root, destination.ID)
					if err != nil || !strings.Contains(text, "Finish the current feature") {
						t.Fatal("wrong Claude continuation", err)
					}
					return
				}
				t.Fatal("Claude destination missing")
			}
		})
	}
}

func TestSessionHandoffValidatesBeforeStoppingSource(t *testing.T) {
	s, db := newTestServerWithStore(t, &continuationRunner{})
	const account = "12345678-1234-4123-8123-123456789abc"
	seedRunnableSlot(t, s, db, "handoff", account)
	for _, target := range []struct {
		provider model.Provider
		account  string
	}{{model.ProviderClaude, account}, {model.ProviderClaude, "missing"}, {model.ProviderCodex, account}, {"unknown", ""}} {
		if _, _, err := s.handoffSlotOp(t.Context(), "handoff", target.provider, target.account, false); err == nil {
			t.Fatal("invalid destination accepted")
		}
		remote, _ := db.GetRemoteSession(t.Context(), "handoff")
		if remote.Desired != model.DesiredRunning {
			t.Fatal("invalid destination stopped source")
		}
	}
}
