package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestContinuationIsPrivateAndIsolatedBySlot(t *testing.T) {
	root := t.TempDir()
	if err := SaveContinuation(root, "slot-a", "private checkpoint"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "continuations", "slot-a"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("checkpoint must be private", err)
	}
	if value, err := LoadContinuation(root, "slot-b"); err != nil || value != "" {
		t.Fatal("checkpoint leaked to another slot")
	}
	if err := SaveContinuation(root, "../escape", "bad"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if err := RemoveContinuation(root, "slot-a"); err != nil {
		t.Fatal(err)
	}
	if value, err := LoadContinuation(root, "slot-a"); err != nil || value != "" {
		t.Fatal("delivered checkpoint remains")
	}
}

func TestAutomaticContinuationAttachesContextOnlyToStartupPrompt(t *testing.T) {
	database := openCheckpointStore(t)
	defer database.Close()
	service := NewService(database, staticGitCapturer{})
	for _, prompt := range []string{ContinuationPrompt, "unrelated prompt", "continue"} {
		payload, err := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "session_id": "2d758a1d-3630-4765-9f84-e91a8de810c7", "cwd": t.TempDir(), "prompt": prompt})
		if err != nil {
			t.Fatal(err)
		}
		response, err := service.HandleStdin(context.Background(), model.ProviderClaude, HookOrigin{
			AccountID: "destination", SlotID: "slot-a", Continuation: "checkpoint with the last unfinished task",
		}, strings.NewReader(string(payload)))
		if err != nil {
			t.Fatal(err)
		}
		if response.ContinuationDelivered != (prompt == ContinuationPrompt) {
			t.Fatal("wrong prompt consumed continuation")
		}
		if prompt == ContinuationPrompt && !strings.Contains(string(response.Output), "last unfinished task") {
			t.Fatal("checkpoint missing from hook output")
		}
	}
}
