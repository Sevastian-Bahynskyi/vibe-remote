package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func TestRetentionSettingPersistsAndProtectsCheckpoints(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if days, err := s.retentionDays(); err != nil || days != 7 {
		t.Fatal("default", days, err)
	}
	for _, value := range []string{"0", "-1", "3651", "abc", "1.5"} {
		if _, err := parseRetentionDays(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, test := range []struct {
		name    string
		age     int
		pinned  bool
		state   model.SessionState
		removed bool
	}{
		{"old", 8, false, model.SessionStopped, true},
		{"recent", 6, false, model.SessionStopped, false},
		{"kept", 20, true, model.SessionStopped, false},
		{"working", 20, false, model.SessionPrompted, false},
	} {
		session, err := s.store.UpsertSession(ctx, model.Session{Provider: model.ProviderClaude, NativeSessionID: test.name, Title: test.name,
			WorkspacePath: t.TempDir(), UpdatedAt: time.Now().Add(-time.Duration(test.age) * 24 * time.Hour), Pinned: test.pinned, State: test.state})
		if err != nil {
			t.Fatal(err)
		}
		s.sweepClosedSessions(ctx)
		_, err = s.store.GetSession(ctx, session.ID)
		if errors.Is(err, store.ErrNotFound) != test.removed {
			t.Fatalf("%s: %v", test.name, err)
		}
	}
	if err := s.saveRetentionDays(14); err != nil {
		t.Fatal(err)
	}
	reloaded := &Server{layout: s.layout}
	if days, err := reloaded.retentionDays(); err != nil || days != 14 {
		t.Fatal("saved setting", days, err)
	}
}

func TestConversationHandoffExcludesCurrentAccount(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	for _, id := range []string{"source", "other"} {
		if _, err := s.store.UpsertAccount(ctx, model.Account{ID: id, Email: id + "@example.com", ProfileDir: t.TempDir(), Status: model.AccountAuthenticated}); err != nil {
			t.Fatal(err)
		}
	}
	session, err := s.store.UpsertSession(ctx, model.Session{Provider: model.ProviderClaude, NativeSessionID: "conversation", AccountID: "source", Title: "Example", WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := s.conversationDetail(ctx, session.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Destinations) != 2 {
		t.Fatal("expected Codex and other Claude account", detail.Destinations)
	}
	for _, destination := range detail.Destinations {
		if destination.AccountID == "source" {
			t.Fatal("current account offered")
		}
	}
}

func TestRetentionFormSavesAndRejectsInvalidValues(t *testing.T) {
	s := newTestServer(t)
	for _, test := range []struct {
		value  string
		status int
		days   int
	}{
		{"14", http.StatusOK, 14}, {"0", http.StatusBadRequest, 14},
	} {
		request := httptest.NewRequest(http.MethodPost, "/ui/retention", strings.NewReader("days="+test.value))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		s.uiRetention(response, request)
		if response.Code != test.status {
			t.Fatalf("status %d: %s", response.Code, response.Body.String())
		}
		if days, err := s.retentionDays(); err != nil || days != test.days {
			t.Fatal(days, err)
		}
		if !strings.Contains(response.Body.String(), `value="14"`) {
			t.Fatal("saved setting missing from System form")
		}
	}
}
