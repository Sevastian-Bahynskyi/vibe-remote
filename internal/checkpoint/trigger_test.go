package checkpoint

import (
	"strings"
	"testing"
)

func TestIsContinueRequest(t *testing.T) {
	t.Parallel()
	accepted := []string{
		"continue",
		"Continue.",
		"/continue",
		"please continue",
		"resume from the checkpoint",
		"continue the refactor",
		"okay, now just continued!",
		"(please) [resume] this work",
	}
	for _, prompt := range accepted {
		if !IsContinueRequest(prompt) {
			t.Errorf("IsContinueRequest(%q) = false, want true", prompt)
		}
	}

	rejected := []string{
		"",
		"please",
		"can you continue",
		"continuing the refactor",
		"continue one two three four five six",
		"/please/continue",
		"continue " + strings.Repeat("x", 192),
	}
	for _, prompt := range rejected {
		if IsContinueRequest(prompt) {
			t.Errorf("IsContinueRequest(%q) = true, want false", prompt)
		}
	}
}
