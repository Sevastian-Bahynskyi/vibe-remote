package checkpoint

import (
	"errors"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func TestParseHookEventAcceptsClaudeAndCodexFieldNames(t *testing.T) {
	t.Parallel()

	claude, err := ParseHookEvent(model.ProviderClaude, []byte(`{
		"session_id":"claude-session",
		"hook_event_name":"UserPromptSubmit",
		"prompt":"continue",
		"cwd":"/workspace"
	}`))
	if err != nil {
		t.Fatalf("parse Claude hook: %v", err)
	}
	if claude.Kind != store.HookEventPrompt || claude.NativeSessionID != "claude-session" || claude.Prompt != "continue" {
		t.Fatalf("unexpected Claude hook: %#v", claude)
	}

	codex, err := ParseHookEvent(model.ProviderCodex, []byte(`{
		"sessionId":"codex-session",
		"turnId":"turn-1",
		"hookEventName":"Stop",
		"lastAssistantMessage":"done",
		"model":"gpt-5.6-sol",
		"effort":{"level":"high"},
		"cwd":"/workspace"
	}`))
	if err != nil {
		t.Fatalf("parse Codex hook: %v", err)
	}
	if codex.Kind != store.HookEventStop || codex.NativeTurnID != "turn-1" || codex.AssistantMessage != "done" ||
		codex.Model != "" || codex.Effort != "" {
		t.Fatalf("unexpected Codex hook: %#v", codex)
	}
}

func TestParseCodexNotify(t *testing.T) {
	t.Parallel()

	event, err := ParseCodexNotify([]byte(`{
		"type":"agent-turn-complete",
		"thread-id":"thread-1",
		"turn-id":"turn-1",
		"cwd":"/workspace",
		"input-messages":["first", "steering"],
		"last-assistant-message":"finished"
	}`))
	if err != nil {
		t.Fatalf("parse Codex notification: %v", err)
	}
	if event.Kind != store.HookEventComplete || event.Prompt != "first\nsteering" || event.AssistantMessage != "finished" {
		t.Fatalf("unexpected Codex notification: %#v", event)
	}

	_, err = ParseCodexNotify([]byte(`{"type":"approval-requested"}`))
	if !errors.Is(err, ErrUnsupportedEvent) {
		t.Fatalf("unsupported notification error = %v", err)
	}
}

func TestParseClaudeStopFailure(t *testing.T) {
	t.Parallel()

	event, err := ParseHookEvent(model.ProviderClaude, []byte(`{
		"session_id":"claude-session",
		"hook_event_name":"StopFailure",
		"cwd":"/workspace"
	}`))
	if err != nil {
		t.Fatalf("parse Claude failure hook: %v", err)
	}
	if event.Kind != store.HookEventFailure {
		t.Fatalf("kind = %q, want failure", event.Kind)
	}
}

func TestParseClaudeSessionStart(t *testing.T) {
	t.Parallel()

	event, err := ParseHookEvent(model.ProviderClaude, []byte(`{
		"session_id":"claude-session",
		"hook_event_name":"SessionStart",
		"source":"startup",
		"model":"claude-opus-4-1",
		"effort":{"level":"high"},
		"cwd":"/workspace"
	}`))
	if err != nil {
		t.Fatalf("parse Claude session start: %v", err)
	}
	if event.Kind != store.HookEventSessionStart || event.NativeSessionID != "claude-session" ||
		event.Model != "claude-opus-4-1" || event.Effort != "high" {
		t.Fatalf("unexpected Claude session start: %#v", event)
	}
}

func TestParseClaudeModelSwitchAndDropsInvalidSettings(t *testing.T) {
	t.Parallel()

	switched, err := ParseHookEvent(model.ProviderClaude, []byte(`{
		"session_id":"claude-session",
		"hook_event_name":"PostModelSwitch",
		"from_model":"claude-sonnet-4-5",
		"to_model":"claude-opus-4-1",
		"cwd":"/workspace"
	}`))
	if err != nil {
		t.Fatalf("parse model switch: %v", err)
	}
	if switched.Kind != store.HookEventModelSwitch || switched.Model != "claude-opus-4-1" {
		t.Fatalf("unexpected model switch: %#v", switched)
	}

	invalid, err := ParseHookEvent(model.ProviderClaude, []byte(`{
		"session_id":"claude-session",
		"hook_event_name":"Stop",
		"model":"--dangerously-skip-permissions",
		"effort":{"level":"unlimited"},
		"cwd":"/workspace"
	}`))
	if err != nil {
		t.Fatalf("parse invalid settings payload: %v", err)
	}
	if invalid.Model != "" || invalid.Effort != "" {
		t.Fatalf("invalid settings survived parsing: %#v", invalid)
	}
}
