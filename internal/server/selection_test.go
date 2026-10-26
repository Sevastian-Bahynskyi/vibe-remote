package server

import (
	"context"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func TestSelectedConversationDropsPreviousSlotPin(t *testing.T) {
	runner := &continuationRunner{}
	s, db := newTestServerWithStore(t, runner)
	ctx := context.Background()
	seedRunnableSlot(t, s, db, "slot-a", "12345678-1234-4123-8123-123456789abc")
	remote, _ := db.GetRemoteSession(ctx, "slot-a")
	remote.Model = "sonnet"
	remote.RequestedModel = "sonnet"
	remote.ResumeSessionID = "old-conversation"
	if _, err := db.UpsertRemoteSession(ctx, remote); err != nil {
		t.Fatal(err)
	}
	checkpoint, _, err := db.RecordHookEvent(ctx, store.HookEvent{Provider: model.ProviderClaude, NativeSessionID: "selected-conversation", AccountID: remote.AccountID, WorkspacePath: remote.WorkspacePath, Kind: store.HookEventPrompt, Prompt: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordAgentSettings(ctx, model.ProviderClaude, "selected-conversation", store.AgentSettings{Model: "opus[1m]", Effort: "max"}); err != nil {
		t.Fatal(err)
	}
	conversation := checkpoint.ID
	updated, err := s.updateSlotOp(ctx, remote.ID, remoteSessionRequest{ResumeSessionID: &conversation})
	if err != nil {
		t.Fatal(err)
	}
	if updated.RequestedModel != "opus[1m]" || updated.RequestedEffort != "max" {
		t.Fatalf("selected settings=%+v", updated)
	}
	clear := ""
	updated, err = s.updateSlotOp(ctx, remote.ID, remoteSessionRequest{Model: &clear, Effort: &clear})
	if err != nil {
		t.Fatal(err)
	}
	if updated.RequestedModel != "opus" || updated.RequestedEffort != "high" {
		t.Fatal("clear did not restore shared defaults")
	}
}

func TestContinuationFallsBackToExactSourceSlot(t *testing.T) {
	runner := &continuationRunner{}
	s, db := newTestServerWithStore(t, runner)
	ctx := context.Background()
	seedRunnableSlot(t, s, db, "source", "12345678-1234-4123-8123-123456789abc")
	seedRunnableSlot(t, s, db, "destination", "22345678-1234-4123-8123-123456789abc")
	sourceSlot, _ := db.GetRemoteSession(ctx, "source")
	destination, _ := db.GetRemoteSession(ctx, "destination")
	sourceSlot.ResumeSessionID = "source-conversation"
	sourceSlot.Model = "opus[1m]"
	sourceSlot.Effort = "max"
	sourceSlot.Desired = model.DesiredStopped
	if _, err := db.UpsertRemoteSession(ctx, sourceSlot); err != nil {
		t.Fatal(err)
	}
	source, _, err := db.RecordHookEvent(ctx, store.HookEvent{Provider: model.ProviderClaude, NativeSessionID: sourceSlot.ResumeSessionID, AccountID: sourceSlot.AccountID, WorkspacePath: sourceSlot.WorkspacePath, Kind: store.HookEventPrompt, Prompt: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	handoff, _, err := s.createHandoffOp(ctx, source.ID, model.ProviderClaude, destination.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetRemoteSession(ctx, handoff.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestedModel != "opus[1m]" || got.RequestedEffort != "max" {
		t.Fatalf("source slot settings lost: %+v", got)
	}
}
