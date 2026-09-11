package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

var ErrUnsupportedEvent = errors.New("unsupported hook event")

func ParseHookEvent(provider model.Provider, payload []byte) (store.HookEvent, error) {
	if provider != model.ProviderClaude && provider != model.ProviderCodex {
		return store.HookEvent{}, errors.New("valid hook provider is required")
	}
	object, err := decodeObject(payload)
	if err != nil {
		return store.HookEvent{}, err
	}
	eventName := firstString(object, "hook_event_name", "hookEventName", "type")
	kind, err := parseEventKind(eventName)
	if err != nil {
		return store.HookEvent{}, err
	}
	event := store.HookEvent{
		Provider:         provider,
		NativeSessionID:  firstString(object, "session_id", "sessionId", "thread_id", "threadId", "thread-id"),
		NativeTurnID:     firstString(object, "turn_id", "turnId", "turn-id"),
		WorkspacePath:    canonicalWorkspace(firstString(object, "cwd", "workspace_path", "workspacePath")),
		Kind:             kind,
		Prompt:           firstString(object, "prompt"),
		AssistantMessage: firstString(object, "last_assistant_message", "lastAssistantMessage", "last-assistant-message"),
	}
	if event.NativeSessionID == "" {
		return store.HookEvent{}, errors.New("hook event is missing session id")
	}
	return event, nil
}

func ParseCodexNotify(payload []byte) (store.HookEvent, error) {
	object, err := decodeObject(payload)
	if err != nil {
		return store.HookEvent{}, err
	}
	if firstString(object, "type") != "agent-turn-complete" {
		return store.HookEvent{}, ErrUnsupportedEvent
	}
	prompt := strings.Join(firstStringSlice(object, "input-messages", "input_messages", "inputMessages"), "\n")
	event := store.HookEvent{
		Provider:         model.ProviderCodex,
		NativeSessionID:  firstString(object, "thread-id", "thread_id", "threadId", "session_id", "sessionId"),
		NativeTurnID:     firstString(object, "turn-id", "turn_id", "turnId"),
		WorkspacePath:    canonicalWorkspace(firstString(object, "cwd")),
		Kind:             store.HookEventComplete,
		Prompt:           prompt,
		AssistantMessage: firstString(object, "last-assistant-message", "last_assistant_message", "lastAssistantMessage"),
	}
	if event.NativeSessionID == "" {
		return store.HookEvent{}, errors.New("Codex notification is missing thread id")
	}
	return event, nil
}

func decodeObject(payload []byte) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, fmt.Errorf("decode hook JSON: %w", err)
	}
	if object == nil {
		return nil, errors.New("hook JSON must be an object")
	}
	return object, nil
}

func parseEventKind(name string) (store.HookEventKind, error) {
	switch name {
	case "SessionStart", "session_start", "session-start":
		return store.HookEventSessionStart, nil
	case "UserPromptSubmit", "user_prompt_submit", "user-prompt-submit":
		return store.HookEventPrompt, nil
	case "Stop", "stop":
		return store.HookEventStop, nil
	case "Interrupt", "interrupt":
		return store.HookEventInterrupt, nil
	case "StopFailure", "stop_failure", "stop-failure":
		return store.HookEventFailure, nil
	case "agent-turn-complete", "AgentTurnComplete":
		return store.HookEventComplete, nil
	default:
		return "", ErrUnsupportedEvent
	}
}

func firstString(object map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, found := object[key]
		if !found {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			return value
		}
	}
	return ""
}

func firstStringSlice(object map[string]json.RawMessage, keys ...string) []string {
	for _, key := range keys {
		raw, found := object[key]
		if !found {
			continue
		}
		var values []string
		if err := json.Unmarshal(raw, &values); err == nil {
			return values
		}
	}
	return nil
}
