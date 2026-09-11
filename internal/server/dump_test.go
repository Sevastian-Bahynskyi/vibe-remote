package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

// TestDumpRenderedPages writes every screen to DUMP_DIR for eyeballing. It never
// runs unless DUMP_DIR is set.
//
// Screens are addressed by ?screen= against the one document route, never by
// path — see screenURL for why the document URL may not gain a directory level.
// The status assertion is what stops this list from rotting: a screen that is
// renamed or dropped fails here instead of silently dumping an error page.
func TestDumpRenderedPages(t *testing.T) {
	directory := os.Getenv("DUMP_DIR")
	if directory == "" {
		t.Skip("set DUMP_DIR to dump rendered pages")
	}
	service, database := newTestServerWithStore(t, inertRunner{})
	remote := seedSession(t, database)
	conversation, err := database.UpsertSession(t.Context(), model.Session{
		Provider: model.ProviderClaude, NativeSessionID: "conv-abc", AccountID: remote.AccountID,
		Title: "Rework the onboarding flow", WorkspacePath: remote.WorkspacePath,
		Branch: "feature/onboarding", State: model.SessionCompleted, UpdatedAt: time.Now().Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	screens := []struct{ name, screen, id string }{
		{"home", "home", ""},
		{"session-new", "session-new", ""},
		{"session", "session", remote.ID},
		{"session-edit", "session-edit", remote.ID},
		{"accounts", "accounts", ""},
		{"workspaces", "workspaces", ""},
		{"conversations", "conversations", ""},
		{"conversation", "conversation", conversation.ID},
		{"system", "system", ""},
		{"guide", "guide", ""},
	}
	for _, screen := range screens {
		status, body := get(t, service, "/"+screenURL(screen.screen, screen.id), false)
		if status != http.StatusOK {
			t.Errorf("screen %q: status %d, want %d", screen.screen, status, http.StatusOK)
			continue
		}
		path := filepath.Join(directory, screen.name+".html")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
