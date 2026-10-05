package checkpoint

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func IsContinueRequest(prompt string) bool {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || utf8.RuneCountInString(prompt) > 200 {
		return false
	}
	prompt = strings.ToLower(prompt)
	if strings.HasPrefix(prompt, "/") {
		prompt = strings.TrimPrefix(prompt, "/")
	}
	rawWords := strings.Fields(prompt)
	words := make([]string, 0, len(rawWords))
	for _, word := range rawWords {
		word = strings.TrimFunc(word, unicode.IsPunct)
		if word != "" {
			words = append(words, word)
		}
	}
	for len(words) > 0 && isContinueFiller(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 || len(words) > 6 {
		return false
	}
	switch words[0] {
	case "continue", "continued", "resume":
		return true
	default:
		return false
	}
}

func isContinueFiller(word string) bool {
	switch word {
	case "please", "ok", "okay", "hey", "now", "just":
		return true
	default:
		return false
	}
}
