package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestOpenSecuresAndConfiguresDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "directory with spaces", "state.db")
	database := openTestStore(t, path, time.Now)
	defer database.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %o, want 600", got)
	}

	var journalMode string
	if err := database.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}
	var busyTimeout int
	if err := database.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy timeout = %d, want 5000", busyTimeout)
	}
}

func TestAccountWorkspaceAndSessionCRUD(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)
	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return now })
	defer database.Close()
	ctx := context.Background()

	account, err := database.UpsertAccount(ctx, model.Account{
		ID:         "claude-main",
		Email:      "person@example.com",
		ProfileDir: "/profiles/main",
		Status:     model.AccountAuthenticated,
		Active:     true,
	})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if account.Email != "person@example.com" || account.CreatedAt != now || account.UpdatedAt != now {
		t.Fatalf("unexpected account: %#v", account)
	}
	accounts, err := database.ListAccounts(ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("list accounts: count=%d err=%v", len(accounts), err)
	}

	workspaceA, err := database.UpsertWorkspace(ctx, model.Workspace{ID: "ws-a", Label: "A", Path: "/tmp/a", Selected: true})
	if err != nil {
		t.Fatalf("upsert workspace A: %v", err)
	}
	_, err = database.UpsertWorkspace(ctx, model.Workspace{ID: "ws-b", Label: "B", Path: "/tmp/b", Selected: true})
	if err != nil {
		t.Fatalf("upsert workspace B: %v", err)
	}
	workspaceA, err = database.GetWorkspace(ctx, workspaceA.ID)
	if err != nil {
		t.Fatalf("get workspace A: %v", err)
	}
	if workspaceA.Selected {
		t.Fatal("selecting workspace B did not clear workspace A")
	}

	session, err := database.UpsertSession(ctx, model.Session{
		Provider:        model.ProviderClaude,
		NativeSessionID: "native-session",
		AccountID:       account.ID,
		Title:           "Task",
		WorkspacePath:   "/tmp/a",
		State:           model.SessionPrompted,
		LastPrompt:      "do it",
	})
	if err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	sessions, err := database.ListSessions(ctx, SessionFilter{Provider: model.ProviderClaude, WorkspacePath: "/tmp/a"})
	if err != nil || len(sessions) != 1 || sessions[0].ID != session.ID {
		t.Fatalf("list sessions: %#v err=%v", sessions, err)
	}
	if err := database.DeleteSession(ctx, session.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := database.GetSession(ctx, session.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted session error = %v, want ErrNotFound", err)
	}
	if err := database.DeleteAccount(ctx, account.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if err := database.DeleteWorkspace(ctx, "ws-a"); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
}

func TestRecordHookEventMergesOutOfOrderWithoutDowngrade(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)
	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return base })
	defer database.Close()
	ctx := context.Background()

	events := []HookEvent{
		{
			Provider: model.ProviderCodex, NativeSessionID: "session-1", NativeTurnID: "turn-1",
			WorkspacePath: "/work", Kind: HookEventComplete, Prompt: "fallback prompt",
			AssistantMessage: "finished", OccurredAt: base.Add(3 * time.Second),
		},
		{
			Provider: model.ProviderCodex, NativeSessionID: "session-1", NativeTurnID: "turn-1",
			WorkspacePath: "/work", Kind: HookEventPrompt, Prompt: "exact prompt", OccurredAt: base.Add(time.Second),
		},
		{
			Provider: model.ProviderCodex, NativeSessionID: "session-1", NativeTurnID: "turn-1",
			WorkspacePath: "/work", Kind: HookEventStop, AssistantMessage: "stop copy", OccurredAt: base.Add(2 * time.Second),
		},
	}
	var session model.Session
	var turn model.Turn
	for _, event := range events {
		var err error
		session, turn, err = database.RecordHookEvent(ctx, event)
		if err != nil {
			t.Fatalf("record event %s: %v", event.Kind, err)
		}
	}
	if turn.State != model.SessionCompleted || session.State != model.SessionCompleted {
		t.Fatalf("states = turn:%s session:%s, want completed", turn.State, session.State)
	}
	if turn.Prompt != "exact prompt" {
		t.Fatalf("prompt = %q, want exact hook prompt", turn.Prompt)
	}
	if turn.AssistantMessage != "finished" {
		t.Fatalf("assistant = %q, want completion message", turn.AssistantMessage)
	}
	if !turn.CreatedAt.Equal(base.Add(time.Second)) || !turn.UpdatedAt.Equal(base.Add(3*time.Second)) {
		t.Fatalf("unexpected turn times: created=%s updated=%s", turn.CreatedAt, turn.UpdatedAt)
	}
}

func TestRecordHookEventCorrelatesClaudeTurnWithoutNativeTurnID(t *testing.T) {
	t.Parallel()

	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), time.Now)
	defer database.Close()
	ctx := context.Background()

	_, prompted, err := database.RecordHookEvent(ctx, HookEvent{
		Provider: model.ProviderClaude, NativeSessionID: "claude-session", Kind: HookEventPrompt,
		Prompt: "build this", WorkspacePath: "/work",
	})
	if err != nil {
		t.Fatalf("record prompt: %v", err)
	}
	_, stopped, err := database.RecordHookEvent(ctx, HookEvent{
		Provider: model.ProviderClaude, NativeSessionID: "claude-session", Kind: HookEventStop,
		AssistantMessage: "done", WorkspacePath: "/work",
	})
	if err != nil {
		t.Fatalf("record stop: %v", err)
	}
	if prompted.ID != stopped.ID {
		t.Fatalf("prompt turn %q and stop turn %q were not correlated", prompted.ID, stopped.ID)
	}
	turns, err := database.ListTurns(ctx, prompted.SessionID, 0)
	if err != nil || len(turns) != 1 {
		t.Fatalf("list turns: count=%d err=%v", len(turns), err)
	}
}

func TestRecordHookEventCorrelatesOutOfOrderClaudeTurnWithoutNativeTurnID(t *testing.T) {
	t.Parallel()

	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), time.Now)
	defer database.Close()
	ctx := context.Background()

	_, stopped, err := database.RecordHookEvent(ctx, HookEvent{
		Provider: model.ProviderClaude, NativeSessionID: "claude-session", Kind: HookEventStop,
		AssistantMessage: "done", WorkspacePath: "/work",
	})
	if err != nil {
		t.Fatalf("record stop: %v", err)
	}
	_, prompted, err := database.RecordHookEvent(ctx, HookEvent{
		Provider: model.ProviderClaude, NativeSessionID: "claude-session", Kind: HookEventPrompt,
		Prompt: "build this", WorkspacePath: "/work",
	})
	if err != nil {
		t.Fatalf("record delayed prompt: %v", err)
	}
	if stopped.ID != prompted.ID {
		t.Fatalf("stop turn %q and delayed prompt turn %q were not correlated", stopped.ID, prompted.ID)
	}
	if prompted.State != model.SessionStopped || prompted.Prompt != "build this" || prompted.AssistantMessage != "done" {
		t.Fatalf("unexpected merged turn: %#v", prompted)
	}
}

func TestRecordHookFailureMarksPendingTurnUnknown(t *testing.T) {
	t.Parallel()

	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), time.Now)
	defer database.Close()
	ctx := context.Background()

	_, prompted, err := database.RecordHookEvent(ctx, HookEvent{
		Provider: model.ProviderClaude, NativeSessionID: "claude-session", Kind: HookEventPrompt,
		Prompt: "finish the task", WorkspacePath: "/work",
	})
	if err != nil {
		t.Fatalf("record prompt: %v", err)
	}
	session, failed, err := database.RecordHookEvent(ctx, HookEvent{
		Provider: model.ProviderClaude, NativeSessionID: "claude-session", Kind: HookEventFailure,
		WorkspacePath: "/work",
	})
	if err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if failed.ID != prompted.ID {
		t.Fatalf("failure created a different turn: prompted=%q failure=%q", prompted.ID, failed.ID)
	}
	if failed.State != model.SessionUnknown || session.State != model.SessionUnknown {
		t.Fatalf("failure states = turn:%s session:%s, want unknown", failed.State, session.State)
	}
}

func TestConcurrentSessionsInSameWorkspaceRemainSeparate(t *testing.T) {
	t.Parallel()

	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), time.Now)
	defer database.Close()
	ctx := context.Background()
	const count = 16
	var group sync.WaitGroup
	errorsBySession := make(chan error, count)
	for index := 0; index < count; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, _, err := database.RecordHookEvent(ctx, HookEvent{
				Provider: model.ProviderCodex, NativeSessionID: "session-" + string(rune('a'+index)),
				NativeTurnID: "turn", Kind: HookEventPrompt, Prompt: "prompt", WorkspacePath: "/shared",
			})
			errorsBySession <- err
		}(index)
	}
	group.Wait()
	close(errorsBySession)
	for err := range errorsBySession {
		if err != nil {
			t.Fatalf("record concurrent session: %v", err)
		}
	}
	sessions, err := database.ListSessions(ctx, SessionFilter{WorkspacePath: "/shared"})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != count {
		t.Fatalf("session count = %d, want %d", len(sessions), count)
	}
}

func TestSeparateStoreConnectionsCanRecordConcurrently(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	const count = 8
	var group sync.WaitGroup
	results := make(chan error, count)
	for index := 0; index < count; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			database, err := Open(path)
			if err != nil {
				results <- err
				return
			}
			defer database.Close()
			_, _, err = database.RecordHookEvent(ctx, HookEvent{
				Provider: model.ProviderCodex, NativeSessionID: "process-" + string(rune('a'+index)),
				NativeTurnID: "turn", Kind: HookEventPrompt, Prompt: "prompt", WorkspacePath: "/shared",
			})
			results <- err
		}(index)
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("record through separate store: %v", err)
		}
	}

	database := openTestStore(t, path, time.Now)
	defer database.Close()
	sessions, err := database.ListSessions(ctx, SessionFilter{WorkspacePath: "/shared"})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != count {
		t.Fatalf("session count = %d, want %d", len(sessions), count)
	}
}

func TestHandoffExpiresAfter48HoursAndCanOnlyBeConsumedOnce(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)
	current := base
	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return current })
	defer database.Close()
	ctx := context.Background()
	session, err := database.UpsertSession(ctx, model.Session{
		Provider: model.ProviderCodex, NativeSessionID: "source", WorkspacePath: "/work", State: model.SessionCompleted,
	})
	if err != nil {
		t.Fatalf("create source session: %v", err)
	}
	handoff, err := database.CreateHandoff(ctx, CreateHandoffParams{
		SourceSessionID: session.ID, DestinationProvider: model.ProviderClaude,
		DestinationAccountID: "claude-main", WorkspacePath: "/work",
	})
	if err != nil {
		t.Fatalf("create handoff: %v", err)
	}
	if !handoff.ExpiresAt.Equal(base.Add(48 * time.Hour)) {
		t.Fatalf("expiry = %s, want %s", handoff.ExpiresAt, base.Add(48*time.Hour))
	}

	const consumers = 20
	var successes atomic.Int32
	var unexpected atomic.Int32
	var group sync.WaitGroup
	for range consumers {
		group.Add(1)
		go func() {
			defer group.Done()
			consumed, consumeErr := database.ConsumeHandoff(ctx, model.ProviderClaude, "claude-main", "/work")
			if consumeErr == nil {
				if consumed.ID == handoff.ID && consumed.ConsumedAt != nil {
					successes.Add(1)
				} else {
					unexpected.Add(1)
				}
				return
			}
			if !errors.Is(consumeErr, ErrNotFound) {
				unexpected.Add(1)
			}
		}()
	}
	group.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful consumers = %d, want 1", got)
	}
	if got := unexpected.Load(); got != 0 {
		t.Fatalf("unexpected consumer outcomes = %d", got)
	}

	_, err = database.CreateHandoff(ctx, CreateHandoffParams{
		SourceSessionID: session.ID, DestinationProvider: model.ProviderClaude,
		DestinationAccountID: "claude-main", WorkspacePath: "/work",
	})
	if err != nil {
		t.Fatalf("create expiring handoff: %v", err)
	}
	current = base.Add(48*time.Hour + time.Nanosecond)
	if _, err := database.ConsumeHandoff(ctx, model.ProviderClaude, "claude-main", "/work"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("consume expired handoff error = %v, want ErrNotFound", err)
	}
}

func TestHandoffClaimReleaseFinalizeAndSupersession(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)
	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return base })
	defer database.Close()
	ctx := context.Background()
	session, err := database.UpsertSession(ctx, model.Session{Provider: model.ProviderCodex, NativeSessionID: "source", WorkspacePath: "/work", State: model.SessionCompleted})
	if err != nil {
		t.Fatal(err)
	}
	first, err := database.CreateHandoff(ctx, CreateHandoffParams{SourceSessionID: session.ID, DestinationProvider: model.ProviderClaude, DestinationAccountID: "account", WorkspacePath: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := database.ClaimHandoffByID(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.FindHandoff(ctx, model.ProviderClaude, "account", "/work"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claimed handoff remained discoverable: %v", err)
	}
	if _, err := database.CreateHandoff(ctx, CreateHandoffParams{SourceSessionID: session.ID, DestinationProvider: model.ProviderClaude, DestinationAccountID: "account", WorkspacePath: "/work"}); err == nil {
		t.Fatal("created a replacement while delivery was claimed")
	}
	if err := database.ReleaseHandoffClaim(ctx, first.ID, claim.Token); err != nil {
		t.Fatal(err)
	}
	second, err := database.CreateHandoff(ctx, CreateHandoffParams{SourceSessionID: session.ID, DestinationProvider: model.ProviderClaude, DestinationAccountID: "account", WorkspacePath: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if available, err := database.FindHandoff(ctx, model.ProviderClaude, "account", "/work"); err != nil || available.ID != second.ID {
		t.Fatalf("newest handoff unavailable: %#v %v", available, err)
	}
	secondClaim, err := database.ClaimHandoffByID(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := database.CompleteHandoffClaim(ctx, second.ID, secondClaim.Token)
	if err != nil || completed.ConsumedAt == nil {
		t.Fatalf("claim completion failed: %#v %v", completed, err)
	}
}

func TestSetActiveRoutingHandlesEitherInsertionOrder(t *testing.T) {
	t.Parallel()
	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), time.Now)
	defer database.Close()
	ctx := context.Background()
	for _, account := range []model.Account{
		{ID: "earlier", Email: "earlier@example.com", Status: model.AccountAuthenticated},
		{ID: "later", Email: "later@example.com", Status: model.AccountAuthenticated, Active: true},
	} {
		if _, err := database.UpsertAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	for _, workspace := range []model.Workspace{
		{ID: "earlier", Path: "/tmp/earlier"},
		{ID: "later", Path: "/tmp/later", Selected: true},
	} {
		if _, err := database.UpsertWorkspace(ctx, workspace); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.SetActiveRouting(ctx, "earlier", "earlier"); err != nil {
		t.Fatal(err)
	}
	accounts, _ := database.ListAccounts(ctx)
	workspaces, _ := database.ListWorkspaces(ctx)
	for _, account := range accounts {
		if account.Active != (account.ID == "earlier") {
			t.Fatalf("wrong active account: %#v", accounts)
		}
	}
	for _, workspace := range workspaces {
		if workspace.Selected != (workspace.ID == "earlier") {
			t.Fatalf("wrong selected workspace: %#v", workspaces)
		}
	}
}

func TestSessionPinRenameRetentionAndInterrupt(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, time.September, 6, 12, 0, 0, 0, time.UTC)
	database := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), func() time.Time { return base })
	defer database.Close()
	ctx := context.Background()
	old, err := database.UpsertSession(ctx, model.Session{Provider: model.ProviderClaude, NativeSessionID: "old", WorkspacePath: "/work", State: model.SessionStopped, UpdatedAt: base.Add(-31 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := database.UpsertSession(ctx, model.Session{Provider: model.ProviderClaude, NativeSessionID: "pinned", WorkspacePath: "/work", State: model.SessionStopped, UpdatedAt: base.Add(-31 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err = database.UpdateSessionMetadata(ctx, pinned.ID, "Keep me", true)
	if err != nil || !pinned.Pinned || pinned.Title != "Keep me" {
		t.Fatalf("metadata update failed: %#v %v", pinned, err)
	}
	count, err := database.CleanupClosedSessions(ctx, base.Add(-30*24*time.Hour))
	if err != nil || count != 1 {
		t.Fatalf("cleanup count=%d err=%v", count, err)
	}
	if _, err := database.GetSession(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old session retained: %v", err)
	}
	open, _, err := database.RecordHookEvent(ctx, HookEvent{Provider: model.ProviderClaude, NativeSessionID: "open", NativeTurnID: "turn", AccountID: "account", WorkspacePath: "/work", Kind: HookEventPrompt, Prompt: "work", OccurredAt: base})
	if err != nil {
		t.Fatal(err)
	}
	git := model.GitSnapshot{Root: "/work", Branch: "feature", HeadSHA: "abc", CapturedAt: base}
	if err := database.InterruptSession(ctx, open.ID, git, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	interrupted, err := database.GetSession(ctx, open.ID)
	if err != nil || interrupted.State != model.SessionInterrupted || interrupted.Branch != "feature" {
		t.Fatalf("session was not interrupted: %#v %v", interrupted, err)
	}
}

func openTestStore(t *testing.T, path string, now func() time.Time) *Store {
	t.Helper()
	database, err := OpenWithOptions(path, Options{Now: now, BusyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return database
}
