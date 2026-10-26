package model

import "testing"

func TestClaudeSelectionPrecedenceIsIndependentPerField(t *testing.T) {
	sonnet, clear, invalid := "sonnet[1m]", "", "gpt-6"
	for _, test := range []struct {
		name               string
		op                 SelectionOverride
		conversation, slot AgentSelection
		model, effort      string
		invalid            bool
	}{
		{"defaults", SelectionOverride{}, AgentSelection{}, AgentSelection{}, "opus", "high", false},
		{"source intent", SelectionOverride{}, AgentSelection{RequestedModel: "opus", Model: "haiku", Effort: "max"}, AgentSelection{Model: "sonnet", Effort: "low"}, "opus", "max", false},
		{"independent fallback", SelectionOverride{}, AgentSelection{Model: "claude-opus-4-6[1m]"}, AgentSelection{Effort: "low"}, "claude-opus-4-6[1m]", "low", false},
		{"operation", SelectionOverride{Model: &sonnet}, AgentSelection{Model: "opus", Effort: "max"}, AgentSelection{}, "sonnet[1m]", "max", false},
		{"clear", SelectionOverride{Model: &clear, Effort: &clear}, AgentSelection{Model: "sonnet", Effort: "low"}, AgentSelection{}, "opus", "high", false},
		{"no GPT transfer", SelectionOverride{}, AgentSelection{Model: "gpt-6"}, AgentSelection{}, "opus", "high", false},
		{"reject explicit GPT", SelectionOverride{Model: &invalid}, AgentSelection{}, AgentSelection{}, "", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveClaudeSelection(test.op, test.conversation, test.slot)
			if test.invalid {
				if err == nil {
					t.Fatal("invalid request accepted")
				}
				return
			}
			if err != nil || got.Model != test.model || got.Effort != test.effort {
				t.Fatalf("selection=%+v error=%v", got, err)
			}
		})
	}
}

func TestExtendedContextModelValidation(t *testing.T) {
	for _, name := range []string{"opus[1m]", "sonnet[1m]", "claude-opus-4-6[1m]"} {
		if !ValidClaudeModelName(name) {
			t.Fatalf("rejected %s", name)
		}
	}
	for _, name := range []string{"opus[1m][1m]", "opus[2m]", "--opus", "gpt-6", "opus[1m];echo"} {
		if ValidClaudeModelName(name) {
			t.Fatalf("accepted %s", name)
		}
	}
}
