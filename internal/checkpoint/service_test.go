package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

type staticGitCapturer struct {
	snapshot model.GitSnapshot
	err      error
}

func (capturer staticGitCapturer) Capture(context.Context, string) (model.GitSnapshot, error) {
	return capturer.snapshot, capturer.err
}

func TestContinueConsumesOneHandoffAndEmitsBoundedContext(t *testing.T) {
	t.Parallel()

	database := openCheckpointStore(t)
	defer database.Close()
	ctx := context.Background()
	workspace := filepath.Join(t.TempDir(), "workspace")
	snapshot := model.GitSnapshot{
		Root: workspace, Branch: "feature/handoff", HeadSHA: strings.Repeat("a", 40),
		Status: "1 .M N... file.go", ChangedPaths: []string{"file.go"}, CapturedAt: time.Now(),
	}
	service := NewService(database, staticGitCapturer{snapshot: snapshot})

	var source model.Session
	for index := 0; index < 6; index++ {
		prompt := strings.Repeat("prompt ", 400) + string(rune('a'+index))
		assistant := strings.Repeat("response ", 400) + string(rune('a'+index))
		var err error
		source, _, err = database.RecordHookEvent(ctx, store.HookEvent{
			Provider: model.ProviderCodex, NativeSessionID: "source-session", NativeTurnID: "turn-" + string(rune('a'+index)),
			WorkspacePath: workspace, Kind: store.HookEventComplete, Prompt: prompt,
			AssistantMessage: assistant, Git: snapshot, OccurredAt: time.Now().Add(time.Duration(index) * time.Second),
		})
		if err != nil {
			t.Fatalf("record source turn: %v", err)
		}
	}
	handoff, err := database.CreateHandoff(ctx, store.CreateHandoffParams{
		SourceSessionID: source.ID, DestinationProvider: model.ProviderClaude,
		DestinationAccountID: "claude-main", WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatalf("create handoff: %v", err)
	}

	payload := `{"session_id":"destination","turn_id":"destination-turn","hook_event_name":"UserPromptSubmit","prompt":"  continue\n","cwd":` + quotedJSON(workspace) + `}`
	response, err := service.HandleStdin(ctx, model.ProviderClaude, HookOrigin{AccountID: "claude-main"}, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("handle continue: %v", err)
	}
	if response.Consumed != nil {
		t.Fatalf("handoff was consumed before output delivery: %#v", response.Consumed)
	}
	if err := response.Finalize(ctx); err != nil {
		t.Fatalf("finalize handoff: %v", err)
	}
	if response.Consumed == nil || response.Consumed.ID != handoff.ID {
		t.Fatalf("consumed handoff = %#v, want %s", response.Consumed, handoff.ID)
	}
	var output promptHookOutput
	if err := json.Unmarshal(response.Output, &output); err != nil {
		t.Fatalf("decode hook output: %v", err)
	}
	if output.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Fatalf("hook event name = %q", output.HookSpecificOutput.HookEventName)
	}
	contextText := output.HookSpecificOutput.AdditionalContext
	if utf8.RuneCountInString(contextText) > MaxContextChars {
		t.Fatalf("context contains %d chars, max %d", utf8.RuneCountInString(contextText), MaxContextChars)
	}
	if !strings.Contains(contextText, "turn-f") || !strings.Contains(contextText, "feature/handoff") || !strings.Contains(contextText, "file.go") {
		t.Fatalf("context is missing latest turn or git state: %q", contextText)
	}

	second, err := service.HandleStdin(ctx, model.ProviderClaude, HookOrigin{AccountID: "claude-main"}, strings.NewReader(`{
		"session_id":"destination-2",
		"turn_id":"destination-turn-2",
		"hook_event_name":"UserPromptSubmit",
		"prompt":"continue",
		"cwd":`+quotedJSON(workspace)+`
	}`))
	if err != nil {
		t.Fatalf("handle second continue: %v", err)
	}
	if len(second.Output) != 0 || second.Consumed != nil {
		t.Fatalf("one-use handoff was reused: %#v", second)
	}
}

func TestOnlyExactTrimmedLowercaseContinueConsumesHandoff(t *testing.T) {
	t.Parallel()

	database := openCheckpointStore(t)
	defer database.Close()
	ctx := context.Background()
	workspace := filepath.Join(t.TempDir(), "workspace")
	service := NewService(database, staticGitCapturer{snapshot: model.GitSnapshot{Root: workspace, CapturedAt: time.Now()}})
	source, err := database.UpsertSession(ctx, model.Session{
		Provider: model.ProviderCodex, NativeSessionID: "source", WorkspacePath: workspace, State: model.SessionStopped,
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	_, err = database.CreateHandoff(ctx, store.CreateHandoffParams{
		SourceSessionID: source.ID, DestinationProvider: model.ProviderClaude,
		DestinationAccountID: "claude-main", WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatalf("create handoff: %v", err)
	}

	response, err := service.HandleStdin(ctx, model.ProviderClaude, HookOrigin{AccountID: "claude-main"}, strings.NewReader(`{
		"session_id":"destination",
		"turn_id":"turn",
		"hook_event_name":"UserPromptSubmit",
		"prompt":"Continue",
		"cwd":`+quotedJSON(workspace)+`
	}`))
	if err != nil {
		t.Fatalf("handle non-matching prompt: %v", err)
	}
	if len(response.Output) != 0 || response.Consumed != nil {
		t.Fatalf("non-exact prompt consumed handoff: %#v", response)
	}
	if _, err := database.ConsumeHandoff(ctx, model.ProviderClaude, "claude-main", workspace); err != nil {
		t.Fatalf("handoff was not left available: %v", err)
	}
}

// The first prompt in a slot adopts that conversation, so a later daemon
// restart resumes it instead of opening an empty one. The slot ID travels in
// the hook's environment because two slots may share an account and workspace.
func TestSlotHookAdoptsTheConversationItIsTalkingIn(t *testing.T) {
	t.Parallel()

	database := openCheckpointStore(t)
	defer database.Close()
	ctx := context.Background()
	workspace := t.TempDir()
	account, err := database.UpsertAccount(ctx, model.Account{Email: "person@example.com", Status: model.AccountAuthenticated})
	if err != nil {
		t.Fatal(err)
	}
	thinga, err := database.UpsertRemoteSession(ctx, model.RemoteSession{
		Name: "Thinga", AccountID: account.ID, WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := database.UpsertRemoteSession(ctx, model.RemoteSession{
		Name: "Sibling", AccountID: account.ID, WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(database, staticGitCapturer{snapshot: model.GitSnapshot{Root: workspace, CapturedAt: time.Now()}})

	prompt := func(slotID, conversation string) {
		t.Helper()
		payload := `{"session_id":` + quotedJSON(conversation) + `,"turn_id":"turn-1","hook_event_name":"UserPromptSubmit","prompt":"work","cwd":` + quotedJSON(workspace) + `}`
		if _, err := service.HandleStdin(ctx, model.ProviderClaude,
			HookOrigin{AccountID: account.ID, SlotID: slotID}, strings.NewReader(payload)); err != nil {
			t.Fatalf("handle prompt: %v", err)
		}
	}
	prompt(thinga.ID, "conversation-thinga")
	prompt(sibling.ID, "conversation-sibling")

	linked, err := database.GetRemoteSession(ctx, thinga.ID)
	if err != nil || linked.ResumeSessionID != "conversation-thinga" {
		t.Fatalf("slot did not adopt its conversation: %#v, %v", linked, err)
	}
	siblingLinked, err := database.GetRemoteSession(ctx, sibling.ID)
	if err != nil || siblingLinked.ResumeSessionID != "conversation-sibling" {
		t.Fatalf("sibling slot took the wrong conversation: %#v, %v", siblingLinked, err)
	}
}

func TestSlotAdoptsConversationAtSessionStart(t *testing.T) {
	t.Parallel()

	database := openCheckpointStore(t)
	defer database.Close()
	ctx := context.Background()
	workspace := t.TempDir()
	account, err := database.UpsertAccount(ctx, model.Account{Email: "person@example.com", Status: model.AccountAuthenticated})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := database.UpsertRemoteSession(ctx, model.RemoteSession{
		Name: "Project", AccountID: account.ID, WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(database, staticGitCapturer{})
	payload := `{"session_id":"conversation-at-start","hook_event_name":"SessionStart","source":"startup","cwd":` + quotedJSON(workspace) + `}`

	response, err := service.HandleStdin(ctx, model.ProviderClaude,
		HookOrigin{AccountID: account.ID, SlotID: slot.ID}, strings.NewReader(payload))
	if err != nil {
		t.Fatalf("handle session start: %v", err)
	}
	if !response.Ignored {
		t.Fatal("session start was recorded as a checkpoint turn")
	}
	linked, err := database.GetRemoteSession(ctx, slot.ID)
	if err != nil || linked.ResumeSessionID != "conversation-at-start" {
		t.Fatalf("slot did not adopt conversation at session start: %#v, %v", linked, err)
	}
}

// A hook that carries no slot is an agent this service did not start, such as a
// Claude session the user ran by hand. It is still checkpointed, but it must not
// claim any slot's conversation.
func TestHookWithoutASlotLinksNothing(t *testing.T) {
	t.Parallel()

	database := openCheckpointStore(t)
	defer database.Close()
	ctx := context.Background()
	workspace := t.TempDir()
	account, err := database.UpsertAccount(ctx, model.Account{Email: "person@example.com", Status: model.AccountAuthenticated})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := database.UpsertRemoteSession(ctx, model.RemoteSession{
		Name: "Thinga", AccountID: account.ID, WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(database, staticGitCapturer{snapshot: model.GitSnapshot{Root: workspace, CapturedAt: time.Now()}})

	response, err := service.HandleStdin(ctx, model.ProviderClaude, HookOrigin{AccountID: account.ID}, strings.NewReader(
		`{"session_id":"hand-run","turn_id":"turn-1","hook_event_name":"UserPromptSubmit","prompt":"work","cwd":`+quotedJSON(workspace)+`}`))
	if err != nil {
		t.Fatalf("handle prompt: %v", err)
	}
	if response.Session.NativeSessionID != "hand-run" {
		t.Fatalf("the conversation was not checkpointed: %#v", response.Session)
	}
	unlinked, err := database.GetRemoteSession(ctx, slot.ID)
	if err != nil || unlinked.ResumeSessionID != "" {
		t.Fatalf("a slotless hook claimed a slot: %#v, %v", unlinked, err)
	}
}

func TestHandleStdinIgnoresUnknownEventAndDoesNotLogPayload(t *testing.T) {
	t.Parallel()

	database := openCheckpointStore(t)
	defer database.Close()
	service := NewService(database, staticGitCapturer{})
	response, err := service.HandleStdin(context.Background(), model.ProviderClaude, HookOrigin{AccountID: "account"}, strings.NewReader(`{
		"session_id":"session",
		"hook_event_name":"Notification",
		"secret":"must-not-appear"
	}`))
	if err != nil {
		t.Fatalf("ignore event: %v", err)
	}
	if !response.Ignored {
		t.Fatal("unknown event was not marked ignored")
	}
}

func TestGitCapturerReadsRepositoryWithoutChangingIt(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "untracked file.txt"), []byte("content"), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	capturer := NewGitCapturer()
	snapshot, err := capturer.Capture(context.Background(), root)
	if err != nil {
		t.Fatalf("capture git: %v", err)
	}
	expectedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve test root: %v", err)
	}
	if snapshot.Root != expectedRoot {
		t.Fatalf("root = %q, want %q", snapshot.Root, expectedRoot)
	}
	if len(snapshot.ChangedPaths) != 1 || snapshot.ChangedPaths[0] != "untracked file.txt" {
		t.Fatalf("changed paths = %#v", snapshot.ChangedPaths)
	}
	if !strings.Contains(snapshot.Status, "untracked file.txt") {
		t.Fatalf("status = %q", snapshot.Status)
	}
	if snapshot.CapturedAt.IsZero() {
		t.Fatal("capture time is zero")
	}
}

func TestServiceStillRecordsWhenGitCaptureFails(t *testing.T) {
	t.Parallel()

	database := openCheckpointStore(t)
	defer database.Close()
	service := NewService(database, staticGitCapturer{err: errors.New("git unavailable")})
	response, err := service.HandleStdin(context.Background(), model.ProviderCodex, HookOrigin{}, strings.NewReader(`{
		"session_id":"session",
		"turn_id":"turn",
		"hook_event_name":"UserPromptSubmit",
		"prompt":"work",
		"cwd":"/workspace"
	}`))
	if err != nil {
		t.Fatalf("handle event with unavailable git: %v", err)
	}
	sessions, listErr := database.ListSessions(context.Background(), store.SessionFilter{})
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 1 || response.Session.ID != sessions[0].ID {
		t.Fatalf("event was not recorded without git: response=%#v sessions=%#v", response, sessions)
	}
	if response.Turn.GitJSON == "" {
		t.Fatal("fallback capture timestamp was not recorded")
	}
}

func openCheckpointStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return database
}

func quotedJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func runGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
