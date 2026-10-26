package model

import (
	"errors"
	"strings"
)

type AgentSelection struct {
	Model           string
	Effort          string
	RequestedModel  string
	RequestedEffort string
}

type SelectionOverride struct {
	Model  *string
	Effort *string
}

func ValidClaudeModelName(value string) bool {
	if !ValidModelName(value) {
		return false
	}
	base := strings.TrimSuffix(value, "[1m]")
	switch base {
	case "opus", "sonnet", "haiku", "opusplan", "best", "fable":
		return true
	default:
		return strings.HasPrefix(base, "claude-")
	}
}

func ResolveClaudeSelection(operation SelectionOverride, conversation, slot AgentSelection) (AgentSelection, error) {
	result := AgentSelection{Model: "opus", Effort: "high"}
	for _, candidate := range []string{conversation.RequestedModel, conversation.Model, slot.RequestedModel, slot.Model} {
		if ValidClaudeModelName(candidate) {
			result.Model = candidate
			break
		}
	}
	for _, candidate := range []string{conversation.RequestedEffort, conversation.Effort, slot.RequestedEffort, slot.Effort} {
		if ValidEffortLevel(candidate) {
			result.Effort = candidate
			break
		}
	}
	if operation.Model != nil {
		value := strings.TrimSpace(*operation.Model)
		if value == "" {
			result.Model = "opus"
		} else if !ValidClaudeModelName(value) {
			return AgentSelection{}, errors.New("choose a valid Claude model")
		} else {
			result.Model = value
		}
	}
	if operation.Effort != nil {
		value := strings.TrimSpace(*operation.Effort)
		if value == "" {
			result.Effort = "high"
		} else if !ValidEffortLevel(value) {
			return AgentSelection{}, errors.New("choose a valid effort level")
		} else {
			result.Effort = value
		}
	}
	result.RequestedModel, result.RequestedEffort = result.Model, result.Effort
	return result, nil
}
