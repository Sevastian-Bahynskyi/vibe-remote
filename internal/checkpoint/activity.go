package checkpoint

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func (s *Service) observeActivity(ctx context.Context, origin HookOrigin, payload []byte) (bool, error) {
	var input struct {
		Event         string `json:"hook_event_name"`
		SessionID     string `json:"session_id"`
		Workspace     string `json:"cwd"`
		ToolName      string `json:"tool_name"`
		ToolID        string `json:"tool_use_id"`
		ServerName    string `json:"mcp_server_name"`
		ElicitationID string `json:"elicitation_id"`
		AgentID       string `json:"agent_id"`
	}
	if err := json.Unmarshal(payload, &input); err != nil {
		return false, err
	}
	onlyActivity := false
	switch input.Event {
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionDenied", "Elicitation", "ElicitationResult", "SessionEnd":
		onlyActivity = true
	case "SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "Interrupt":
	default:
		return false, nil
	}
	if origin.ActivityRunID == "" || origin.SlotID == "" {
		return onlyActivity, nil
	}
	if input.AgentID != "" {
		switch input.Event {
		case "SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "Interrupt", "SessionEnd":
			return true, nil
		}
	}
	name, requestID := input.ToolName, input.ToolID
	if input.Event == "Elicitation" || input.Event == "ElicitationResult" {
		name, requestID = input.ServerName, input.ElicitationID
		if requestID == "" {
			requestID = "server:" + name
		}
	}
	err := s.repository.RecordWorkerActivity(ctx, store.ActivityEvent{
		SlotID: origin.SlotID, RunID: origin.ActivityRunID, AccountID: origin.AccountID,
		SessionID: input.SessionID, WorkspacePath: canonicalWorkspace(input.Workspace),
		Event: input.Event, RequestID: requestID, Name: name, OccurredAt: s.now().UTC().Truncate(time.Microsecond),
	})
	return onlyActivity, err
}
