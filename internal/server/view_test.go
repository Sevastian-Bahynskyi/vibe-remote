package server

import (
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	systemstate "github.com/Sevastian-Bahynskyi/vibe-remote/internal/system"
)

func TestSessionStatusCoversEveryWorkerState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state   string
		running bool
		desired string
		label   string
		tone    Tone
	}{
		{"running", true, model.DesiredRunning, "Running", ToneGood},
		{"running", false, model.DesiredRunning, "Not connected", ToneWarn},
		{"starting", false, model.DesiredRunning, "Starting", ToneBusy},
		{"connecting", false, model.DesiredRunning, "Starting", ToneBusy},
		{"restarting", false, model.DesiredRunning, "Restarting", ToneBusy},
		{"stopping", true, model.DesiredRunning, "Stopping", ToneBusy},
		{"failed", false, model.DesiredRunning, "Failed", ToneBad},
		{"stopped", false, model.DesiredStopped, "Stopped", ToneNeutral},
		{"stopped", false, model.DesiredRunning, "Not connected", ToneWarn},
		{"", false, model.DesiredStopped, "Stopped", ToneNeutral},
	}
	for _, testCase := range cases {
		label, tone := sessionStatus(model.WorkerStatus{State: testCase.state, Running: testCase.running}, testCase.desired)
		if label != testCase.label || tone != testCase.tone {
			t.Errorf("state %q running=%v desired=%q = (%q, %q), want (%q, %q)",
				testCase.state, testCase.running, testCase.desired, label, tone, testCase.label, testCase.tone)
		}
	}
}

// A state the manager gains later must surface rather than render as an empty
// badge, so the fallthrough is part of the contract.
func TestSessionStatusSurfacesAnUnknownState(t *testing.T) {
	t.Parallel()
	label, tone := sessionStatus(model.WorkerStatus{State: "reticulating"}, model.DesiredRunning)
	if label != "Reticulating" || tone != ToneNeutral {
		t.Fatalf("unknown state = (%q, %q), want (\"Reticulating\", neutral)", label, tone)
	}
}

func TestOpenDesktopAvailability(t *testing.T) {
	t.Parallel()
	live := model.WorkerStatus{Running: true, RemoteURL: "https://claude.ai/code/cse_01ABC"}
	cases := []struct {
		name             string
		worker           model.WorkerStatus
		local            bool
		desktopInstalled bool
		want             bool
	}{
		{"everything present", live, true, true, true},
		{"asked from the tailnet", live, false, true, false},
		{"desktop app absent", live, true, false, false},
		{"worker not running", model.WorkerStatus{RemoteURL: live.RemoteURL}, true, true, false},
		{"no link yet", model.WorkerStatus{Running: true}, true, true, false},
		{"link is not a Claude URL", model.WorkerStatus{Running: true, RemoteURL: "https://evil.example/code/x"}, true, true, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := canOpenDesktop(testCase.worker, testCase.local, testCase.desktopInstalled); got != testCase.want {
				t.Fatalf("canOpenDesktop = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestSessionViewResolvesAccountAndWorkspace(t *testing.T) {
	t.Parallel()
	state := dashboardState{
		Accounts:   []model.Account{{ID: "acc1", Email: "me@example.com", Status: model.AccountAuthenticated}},
		Workspaces: []model.Workspace{{ID: "ws1", Label: "CoupleGoAI", Path: "/Users/x/dev/cga"}},
		ConversationTitles: map[string]string{
			"conv-1": "Fix the login screen",
		},
	}
	remote := model.RemoteSession{
		ID: "rs1", Name: "Tablet", AccountID: "acc1", WorkspaceID: "ws1",
		WorkspacePath: "/Users/x/dev/cga", ResumeSessionID: "conv-1", Desired: model.DesiredRunning,
		Worker: model.WorkerStatus{Running: true, State: "running"},
	}

	view := buildSession(state, remote)
	if view.AccountEmail != "me@example.com" || view.AccountMissing {
		t.Fatalf("account = %q missing=%v", view.AccountEmail, view.AccountMissing)
	}
	if view.WorkspaceLabel != "CoupleGoAI" {
		t.Fatalf("workspace label = %q", view.WorkspaceLabel)
	}
	if view.ConversationLabel != "Continues Fix the login screen" {
		t.Fatalf("conversation label = %q", view.ConversationLabel)
	}
}

// A slot can outlive the account it was pinned to. The card must say so instead
// of rendering a blank line, which is what the old dashboard did.
func TestSessionViewReportsAMissingAccount(t *testing.T) {
	t.Parallel()
	state := dashboardState{
		Workspaces: []model.Workspace{{ID: "ws1", Label: "Work", Path: "/tmp/work"}},
	}
	remote := model.RemoteSession{ID: "rs1", AccountID: "gone", WorkspaceID: "ws1", WorkspacePath: "/tmp/work"}

	view := buildSession(state, remote)
	if !view.AccountMissing || view.AccountEmail == "" {
		t.Fatalf("missing account not reported: %+v", view)
	}
}

func TestConversationLabelFallsBackToTheIdentifier(t *testing.T) {
	t.Parallel()
	if got := conversationLabel("", nil); got != "New conversation" {
		t.Fatalf("unlinked slot = %q", got)
	}
	got := conversationLabel("2d758a1d-3630-4765-9f84-e91a8de810c7", nil)
	if got != "Continues 2d758a1d-36…" {
		t.Fatalf("unknown title = %q, want a truncated identifier", got)
	}
}

// The conversation tri-state is the semantic most easily lost in a rewrite.
func TestConversationChoicesOfferKeepOnlyWhenTheLinkSurvives(t *testing.T) {
	t.Parallel()
	now := time.Now()
	titles := map[string]string{"conv-1": "Earlier work"}
	resumable := []model.Session{
		{ID: "ses1", NativeSessionID: "conv-9", Title: "Another thread", UpdatedAt: now.Add(-2 * time.Hour)},
	}

	kept := buildConversationChoices(
		ConversationChoiceContext{LinkedID: "conv-1", SameAccount: true, SameWorkspace: true},
		resumable, titles, now)
	if kept[0].Value != conversationKeep || !kept[0].Selected {
		t.Fatalf("keep must be first and preselected when the link survives: %+v", kept[0])
	}
	if kept[1].Value != conversationNew {
		t.Fatalf("second choice = %q, want %q", kept[1].Value, conversationNew)
	}

	moved := buildConversationChoices(
		ConversationChoiceContext{LinkedID: "conv-1", SameAccount: false, SameWorkspace: true},
		resumable, titles, now)
	if moved[0].Value != conversationNew || !moved[0].Selected {
		t.Fatalf("moving to another account must drop the link: %+v", moved[0])
	}
	for _, choice := range moved {
		if choice.Value == conversationKeep {
			t.Fatal("keep was offered after a move, which would resume a conversation in the wrong place")
		}
	}

	fresh := buildConversationChoices(ConversationChoiceContext{IsNewSession: true}, resumable, titles, now)
	if fresh[0].Value != conversationNew || !fresh[0].Selected {
		t.Fatalf("a new session must default to a new conversation: %+v", fresh[0])
	}
}

func TestSessionFormOffersOnlyAuthenticatedAccounts(t *testing.T) {
	t.Parallel()
	accounts := []model.Account{
		{ID: "a", Email: "good@example.com", Status: model.AccountAuthenticated},
		{ID: "b", Email: "pending@example.com", Status: model.AccountPending},
		{ID: "c", Email: "broken@example.com", Status: model.AccountError},
	}
	choices := buildAccountChoices(accounts, "")
	if len(choices) != 1 || choices[0].Value != "a" {
		t.Fatalf("choices = %+v, want only the authenticated account", choices)
	}
	if !choices[0].Selected {
		t.Fatal("the only usable account must be preselected")
	}
}

func TestWorkspaceChoicesDefaultToTheSelectedWorkspace(t *testing.T) {
	t.Parallel()
	workspaces := []model.Workspace{
		{ID: "w1", Label: "One", Path: "/tmp/one"},
		{ID: "w2", Label: "Two", Path: "/tmp/two", Selected: true},
	}
	choices := buildWorkspaceChoices(workspaces, "")
	if !choices[1].Selected || choices[0].Selected {
		t.Fatalf("the last used workspace must be preselected: %+v", choices)
	}
}

// A healthy system must produce no alerts at all: the status area exists to
// carry problems, not to confirm that nothing is wrong.
func TestAlertsAreSilentWhenHealthy(t *testing.T) {
	t.Parallel()
	healthy := dashboardState{
		Health: systemstate.Health{TailnetOnly: true, CodexHooks: true, OnACPower: true},
	}
	if alerts := buildAlerts(healthy); len(alerts) != 0 {
		t.Fatalf("healthy system produced %d alerts: %+v", len(alerts), alerts)
	}
}

func TestAlertsReportEachProblemOnce(t *testing.T) {
	t.Parallel()
	state := dashboardState{
		Health:       systemstate.Health{TailnetOnly: false, CodexHooks: false, CodexHooksInstalled: true},
		PowerWarning: "This Mac is on battery and may become unreachable.",
		Remotes: []model.RemoteSession{
			{ID: "rs1", Name: "Tablet", Worker: model.WorkerStatus{LastError: "boom"}},
			{ID: "rs2", Name: "Other", Worker: model.WorkerStatus{LastError: "also boom"}},
		},
	}
	alerts := buildAlerts(state)
	if len(alerts) != 4 {
		t.Fatalf("alerts = %d, want 4 (tailnet, hooks, power, one session): %+v", len(alerts), alerts)
	}
	if alerts[1].Message != "Checkpoint hooks not verified yet" {
		t.Fatalf("installed-but-unverified hooks = %q", alerts[1].Message)
	}
	// Several failing sessions must not produce several near-identical lines.
	if alerts[3].Href != screenURL("session", "rs1") {
		t.Fatalf("session alert should link to the first failing session, got %q", alerts[3].Href)
	}
}

func TestSessionListExplainsWhyItCannotStart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		state   dashboardState
		can     bool
		blocker string
	}{
		{"nothing configured", dashboardState{}, false, "Add a Claude account and a workspace first."},
		{
			"no workspace",
			dashboardState{Accounts: []model.Account{{Status: model.AccountAuthenticated}}},
			false, "Add a workspace first.",
		},
		{
			"no usable account",
			dashboardState{
				Accounts:   []model.Account{{Status: model.AccountPending}},
				Workspaces: []model.Workspace{{ID: "w"}},
			},
			false, "Add a Claude account first.",
		},
		{
			"ready",
			dashboardState{
				Accounts:   []model.Account{{Status: model.AccountAuthenticated}},
				Workspaces: []model.Workspace{{ID: "w"}},
			},
			true, "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			view := buildSessionList(testCase.state)
			if view.CanCreate != testCase.can || view.Blocker != testCase.blocker {
				t.Fatalf("CanCreate=%v Blocker=%q, want %v %q", view.CanCreate, view.Blocker, testCase.can, testCase.blocker)
			}
		})
	}
}

func TestHumanSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		at   time.Time
		want string
	}{
		{time.Time{}, ""},
		{now, "just now"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-1 * time.Minute), "1 minute ago"},
		{now.Add(-5 * time.Minute), "5 minutes ago"},
		{now.Add(-1 * time.Hour), "1 hour ago"},
		{now.Add(-3 * time.Hour), "3 hours ago"},
		{now.Add(-25 * time.Hour), "1 day ago"},
		{now.Add(-3 * 24 * time.Hour), "3 days ago"},
	}
	for _, testCase := range cases {
		if got := humanSince(testCase.at, now); got != testCase.want {
			t.Errorf("humanSince(%v) = %q, want %q", testCase.at, got, testCase.want)
		}
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	t.Parallel()
	if got := truncate("héllo wörld", 100); got != "héllo wörld" {
		t.Fatalf("short string changed: %q", got)
	}
	got := truncate("ααααααααααββββββββββ", 10)
	if len([]rune(got)) != 10 {
		t.Fatalf("truncate produced %d runes: %q", len([]rune(got)), got)
	}
	if truncate("one\n\ttwo", 100) != "one two" {
		t.Fatal("truncate must fold whitespace so a multi-line prompt stays on one row")
	}
}

// ---- conversation grouping ----

func TestBuildChatGroupsGroupsCheckpointsUnderTheirChat(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	state := dashboardState{
		Remotes: []model.RemoteSession{
			{ID: "rs_widget", Name: "Widget", WorkspacePath: "/Users/seva/code/couplegoai"},
			{ID: "rs_mom", Name: "Mom fixes", WorkspacePath: "/Users/seva/code/couplegoai"},
		},
		Sessions: []model.Session{
			// Newest overall, and it belongs to the second slot.
			{ID: "s1", SlotID: "rs_mom", Title: "Fix the header", UpdatedAt: now.Add(-time.Minute)},
			{ID: "s2", SlotID: "rs_widget", Title: "Widgets round two", UpdatedAt: now.Add(-time.Hour)},
			{ID: "s3", SlotID: "rs_widget", Title: "Widgets round one", UpdatedAt: now.Add(-2 * time.Hour)},
			// No slot at all, and a slot that no longer exists: both are history.
			{ID: "s4", Title: "Something from before", UpdatedAt: now.Add(-48 * time.Hour)},
			{ID: "s5", SlotID: "rs_deleted", Title: "Chat since deleted", UpdatedAt: now.Add(-72 * time.Hour)},
		},
	}

	groups := buildChatGroups(state, now)
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want one per live chat plus the leftovers: %#v", len(groups), groups)
	}
	if groups[0].Name != "Mom fixes" || groups[0].Count != "1 checkpoint" {
		t.Errorf("first group = %q (%s), want the most recently used chat", groups[0].Name, groups[0].Count)
	}
	if groups[1].Name != "Widget" || groups[1].Count != "2 checkpoints" {
		t.Errorf("second group = %q (%s), want Widget with both its checkpoints", groups[1].Name, groups[1].Count)
	}
	last := groups[2]
	if last.ID != unattributedChatID || last.Live {
		t.Errorf("last group = %#v, want the unattributed group last and not live", last)
	}
	if last.Count != "2 checkpoints" {
		t.Errorf("unattributed count = %s, want the slotless and the deleted-slot checkpoints together", last.Count)
	}
	// A checkpoint inside a live chat carries the chat's name for the screens
	// that show it out of context.
	if groups[1].Conversations[0].ChatName != "Widget" {
		t.Errorf("checkpoint chat name = %q, want Widget", groups[1].Conversations[0].ChatName)
	}
}

func TestBuildChatGroupsPutsPinnedCheckpointsFirstWithinAChat(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	state := dashboardState{
		Remotes: []model.RemoteSession{{ID: "rs_one", Name: "Only chat"}},
		Sessions: []model.Session{
			{ID: "s1", SlotID: "rs_one", Title: "Recent", UpdatedAt: now.Add(-time.Minute)},
			{ID: "s2", SlotID: "rs_one", Title: "Kept", Pinned: true, UpdatedAt: now.Add(-time.Hour)},
		},
	}
	groups := buildChatGroups(state, now)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v", groups)
	}
	if got := groups[0].Conversations[0].Title; got != "Kept" {
		t.Errorf("first checkpoint = %q, want the pinned one", got)
	}
}

func TestFindChatGroupResolvesAndRejects(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	state := dashboardState{
		Remotes:  []model.RemoteSession{{ID: "rs_one", Name: "Only chat"}},
		Sessions: []model.Session{{ID: "s1", SlotID: "rs_one", Title: "A turn", UpdatedAt: now}},
	}
	group, found := findChatGroup(state, now, "rs_one")
	if !found || group.Name != "Only chat" {
		t.Fatalf("findChatGroup(rs_one) = (%#v, %v)", group, found)
	}
	if _, found := findChatGroup(state, now, "rs_missing"); found {
		t.Error("findChatGroup resolved a chat that has no checkpoints and no slot")
	}
}

func TestRefreshCadenceSpeedsUpWhileSomethingIsInFlight(t *testing.T) {
	t.Parallel()
	settled := []SessionView{{Tone: ToneGood}, {Tone: ToneNeutral}}
	if got := refreshCadence(settled); got != idleRefreshMS {
		t.Errorf("settled cadence = %d, want the idle rate %d", got, idleRefreshMS)
	}
	starting := []SessionView{{Tone: ToneGood}, {Tone: ToneBusy}}
	if got := refreshCadence(starting); got != transitionalRefreshM {
		t.Errorf("starting cadence = %d, want the fast rate %d", got, transitionalRefreshM)
	}
}
