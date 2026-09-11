package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

// The view layer turns domain state into display-ready values so the templates
// stay free of logic. Every builder here is a pure function of a dashboardState
// plus an injected clock: none of them read the store, touch HTTP, or call
// time.Now, which is what makes them worth testing directly.
//
// Vocabulary note, because the domain has two things called "session": a
// SessionView is a live Remote Control slot (model.RemoteSession), and a
// ConversationView is a captured checkpoint (model.Session). The dashboard says
// "session" and "conversation" respectively, and never says "checkpoint" at the
// user.

type Tone string

const (
	ToneNeutral Tone = "neutral"
	ToneGood    Tone = "good"
	ToneWarn    Tone = "warn"
	ToneBad     Tone = "bad"
	ToneBusy    Tone = "busy"
)

// SessionView is one live Remote Control slot as the dashboard shows it.
type SessionView struct {
	ID                string
	Name              string
	AccountID         string
	AccountEmail      string
	AccountMissing    bool
	WorkspaceID       string
	WorkspaceLabel    string
	WorkspacePath     string
	ConversationLabel string
	ConversationID    string
	Status            string
	Tone              Tone
	Running           bool
	RemoteURL         string
	CanOpenDesktop    bool
	CanStart          bool
	CanStop           bool
	CanRestart        bool
	PID               int
	LastError         string
}

type SessionListView struct {
	Sessions  []SessionView
	Running   int
	Total     int
	CanCreate bool
	// Blocker explains why a session cannot be started yet, and is empty when
	// one can. It is the only place the sessions screen carries prose.
	Blocker string
}

type AccountView struct {
	ID           string
	Email        string
	Status       string
	Tone         Tone
	SessionCount int
	RunningCount int
	LastError    string
	CanStart     bool
}

type WorkspaceView struct {
	ID           string
	Label        string
	Path         string
	SessionCount int
}

type ConversationView struct {
	ID              string
	Title           string
	Provider        string
	NativeID        string
	State           string
	Tone            Tone
	WorkspacePath   string
	Branch          string
	Ago             string
	Pinned          bool
	SharedWorktree  bool
	ResumeCommand   string
	DesktopGuidance string
	IsClaude        bool
}

// ChoiceView is one option in a radio list. The dashboard uses radio lists
// rather than <select> because a phone shows every option at once and takes one
// tap instead of two.
type ChoiceView struct {
	// Name is the radio group this choice belongs to. It lives on the choice
	// rather than being passed alongside it so the partial takes exactly one
	// argument and no dict helper is needed.
	Name     string
	Value    string
	Label    string
	Detail   string
	Selected bool
	Disabled bool
}

// SessionFormView drives both "start a session" and "change setup"; the two
// differ only in their action URL, their title, and whether keeping the current
// conversation is on offer.
type SessionFormView struct {
	Action        string
	Title         string
	SubmitLabel   string
	SessionID     string
	Name          string
	Accounts      []ChoiceView
	Workspaces    []ChoiceView
	Conversations []ChoiceView
	CanSubmit     bool
	Blocker       string
}

type AlertView struct {
	Message string
	Tone    Tone
	Href    string
}

type HealthItemView struct {
	Label  string
	Detail string
	Tone   Tone
}

type SystemView struct {
	RetentionDays  int
	Items          []HealthItemView
	TailscaleURL   string
	StartedAgo     string
	OnThisMac      bool
	HooksInstalled bool
}

type NoticeView struct {
	Message string
	Tone    Tone
}

type FieldView struct {
	Name  string
	Value string
}

// ConfirmView is the inline confirmation row that replaces a card's buttons.
// It covers both cases that need one: a destructive action the user has not
// confirmed yet, and a stop or restart the backend refused because a Claude turn
// is still running. Fields replay the original submission as hidden inputs, so
// the confirmed action is the same action plus one flag and the client has to
// remember nothing.
type ConfirmView struct {
	Post    string
	Label   string
	Message string
	Cancel  string
	Fields  []FieldView
	Danger  bool
}

// SessionDetailView is the session screen: one session plus, when the last
// action needs confirming, the row that asks.
type SessionDetailView struct {
	Session SessionView
	Confirm *ConfirmView
}

// HandoffDestinationView is one place a conversation can be continued. Codex is
// always offered; each authenticated Claude account is offered separately,
// because a handoff is scoped to the account that will consume it.
type HandoffDestinationView struct {
	Provider  string
	AccountID string
	Label     string
}

type ConversationDetailView struct {
	Conversation ConversationView
	Destinations []HandoffDestinationView
	Confirm      *ConfirmView
}

// CountsView feeds the navigation rows on the home screen. The counts are the
// only numbers there: a row that says how many accounts exist is worth a glance,
// a row that explains what accounts are is not.
type CountsView struct {
	Accounts      int
	Workspaces    int
	Conversations int
}

func buildHandoffDestinations(accounts []model.Account) []HandoffDestinationView {
	destinations := []HandoffDestinationView{{Provider: "codex", Label: "Codex"}}
	for _, account := range authenticatedAccounts(accounts) {
		destinations = append(destinations, HandoffDestinationView{
			Provider:  "claude",
			AccountID: account.ID,
			Label:     account.Email,
		})
	}
	return destinations
}

// ---- slots ----

// sessionStatus maps a worker's raw state onto the one label the dashboard
// shows. An unrecognised state falls through to its own value rather than
// rendering blank, so a state added to the manager is visible instead of silent.
func sessionStatus(worker model.WorkerStatus, desired string) (string, Tone) {
	switch worker.State {
	case "running":
		if worker.Running {
			return "Running", ToneGood
		}
		return "Not connected", ToneWarn
	case "starting", "connecting":
		return "Starting", ToneBusy
	case "restarting":
		return "Restarting", ToneBusy
	case "stopping":
		return "Stopping", ToneBusy
	case "failed":
		return "Failed", ToneBad
	case "stopped", "":
		if desired == model.DesiredRunning {
			return "Not connected", ToneWarn
		}
		return "Stopped", ToneNeutral
	default:
		return sentence(worker.State), ToneNeutral
	}
}

// canOpenDesktop is a four-way condition, and every one of the four matters: the
// request has to come from this Mac, the worker has to be running, its link has
// to be one Claude itself printed, and the app has to be installed.
func canOpenDesktop(worker model.WorkerStatus, local, desktopInstalled bool) bool {
	return local && desktopInstalled && worker.Running && isClaudeRemoteURL(worker.RemoteURL)
}

func buildSession(state dashboardState, remote model.RemoteSession) SessionView {
	accounts := indexAccounts(state.Accounts)
	workspaces := indexWorkspaces(state.Workspaces)
	return buildSessionWith(state, remote, accounts, workspaces)
}

func buildSessionWith(
	state dashboardState,
	remote model.RemoteSession,
	accounts map[string]model.Account,
	workspaces map[string]model.Workspace,
) SessionView {
	worker := remote.Worker
	status, tone := sessionStatus(worker, remote.Desired)
	busy := tone == ToneBusy

	view := SessionView{
		ID:             remote.ID,
		Name:           orDefault(remote.Name, "Unnamed session"),
		AccountID:      remote.AccountID,
		WorkspaceID:    remote.WorkspaceID,
		WorkspacePath:  shortPath(remote.WorkspacePath),
		Status:         status,
		Tone:           tone,
		Running:        worker.Running,
		CanStart:       !worker.Running && !busy,
		CanStop:        worker.Running || busy,
		CanRestart:     worker.Running,
		PID:            worker.PID,
		LastError:      worker.LastError,
		ConversationID: remote.ResumeSessionID,
	}

	if account, ok := accounts[remote.AccountID]; ok {
		view.AccountEmail = account.Email
	} else {
		// A slot whose account was deleted out from under it. Say so rather than
		// rendering an empty line.
		view.AccountMissing = true
		view.AccountEmail = "Account removed"
	}

	if workspace, ok := workspaces[remote.WorkspaceID]; ok {
		view.WorkspaceLabel = orDefault(workspace.Label, filepath.Base(workspace.Path))
	} else {
		view.WorkspaceLabel = filepath.Base(remote.WorkspacePath)
	}

	view.ConversationLabel = conversationLabel(remote.ResumeSessionID, state.ConversationTitles)

	if worker.Running && isClaudeRemoteURL(worker.RemoteURL) {
		view.RemoteURL = worker.RemoteURL
	}
	view.CanOpenDesktop = canOpenDesktop(worker, state.OnThisMac, state.Health.ClaudeDesktop)
	return view
}

func conversationLabel(nativeID string, titles map[string]string) string {
	if nativeID == "" {
		return "New conversation"
	}
	if title := strings.TrimSpace(titles[nativeID]); title != "" {
		return "Continues " + truncate(title, 60)
	}
	return "Continues " + truncate(nativeID, 12)
}

func buildSessionList(state dashboardState) SessionListView {
	accounts := indexAccounts(state.Accounts)
	workspaces := indexWorkspaces(state.Workspaces)
	sessions := make([]SessionView, 0, len(state.Remotes))
	for _, remote := range state.Remotes {
		sessions = append(sessions, buildSessionWith(state, remote, accounts, workspaces))
	}
	view := SessionListView{
		Sessions: sessions,
		Running:  state.Running,
		Total:    len(sessions),
	}
	view.CanSubmitReason(state)
	return view
}

// CanSubmitReason fills CanCreate and Blocker. A session needs an authenticated
// account and a workspace; saying which one is missing is the difference between
// a disabled button and a dead end.
func (v *SessionListView) CanSubmitReason(state dashboardState) {
	hasAccount := len(authenticatedAccounts(state.Accounts)) > 0
	hasWorkspace := len(state.Workspaces) > 0
	v.CanCreate = hasAccount && hasWorkspace
	switch {
	case !hasAccount && !hasWorkspace:
		v.Blocker = "Add a Claude account and a workspace first."
	case !hasAccount:
		v.Blocker = "Add a Claude account first."
	case !hasWorkspace:
		v.Blocker = "Add a workspace first."
	}
}

// ---- accounts ----

func accountStatus(account model.Account) (string, Tone) {
	switch account.Status {
	case model.AccountAuthenticated:
		return "Signed in", ToneGood
	case model.AccountPending:
		return "Waiting for sign-in", ToneWarn
	case model.AccountSignedOut:
		return "Signed out", ToneWarn
	case model.AccountError:
		return "Sign-in problem", ToneBad
	default:
		return sentence(string(account.Status)), ToneNeutral
	}
}

func authenticatedAccounts(accounts []model.Account) []model.Account {
	usable := make([]model.Account, 0, len(accounts))
	for _, account := range accounts {
		if account.Status == model.AccountAuthenticated {
			usable = append(usable, account)
		}
	}
	return usable
}

func buildAccounts(state dashboardState) []AccountView {
	views := make([]AccountView, 0, len(state.Accounts))
	for _, account := range state.Accounts {
		status, tone := accountStatus(account)
		view := AccountView{
			ID:        account.ID,
			Email:     account.Email,
			Status:    status,
			Tone:      tone,
			LastError: account.LastError,
			CanStart:  account.Status == model.AccountAuthenticated,
		}
		for _, remote := range state.Remotes {
			if remote.AccountID != account.ID {
				continue
			}
			view.SessionCount++
			if remote.Worker.Running {
				view.RunningCount++
			}
		}
		views = append(views, view)
	}
	return views
}

// ---- workspaces ----

func buildWorkspaces(state dashboardState) []WorkspaceView {
	views := make([]WorkspaceView, 0, len(state.Workspaces))
	for _, workspace := range state.Workspaces {
		view := WorkspaceView{
			ID:    workspace.ID,
			Label: orDefault(workspace.Label, filepath.Base(workspace.Path)),
			Path:  shortPath(workspace.Path),
		}
		for _, remote := range state.Remotes {
			if remote.WorkspaceID == workspace.ID {
				view.SessionCount++
			}
		}
		views = append(views, view)
	}
	return views
}

// ---- conversations ----

func conversationState(session model.Session) (string, Tone) {
	switch session.State {
	case model.SessionPrompted:
		return "Working", ToneBusy
	case model.SessionCompleted:
		return "Finished", ToneGood
	case model.SessionInterrupted:
		return "Interrupted", ToneWarn
	case model.SessionStopped:
		return "Stopped", ToneNeutral
	default:
		return sentence(string(session.State)), ToneNeutral
	}
}

func buildConversation(session model.Session, now time.Time) ConversationView {
	status, tone := conversationState(session)
	provider := "Codex"
	if session.Provider == model.ProviderClaude {
		provider = "Claude"
	}
	return ConversationView{
		ID:              session.ID,
		Title:           orDefault(truncate(session.Title, 80), "Untitled conversation"),
		Provider:        provider,
		NativeID:        session.NativeSessionID,
		State:           status,
		Tone:            tone,
		WorkspacePath:   shortPath(session.WorkspacePath),
		Branch:          session.Branch,
		Ago:             humanSince(session.UpdatedAt, now),
		Pinned:          session.Pinned,
		SharedWorktree:  session.SharedWorktree,
		ResumeCommand:   session.ResumeCommand,
		DesktopGuidance: session.DesktopGuidance,
		IsClaude:        session.Provider == model.ProviderClaude,
	}
}

func buildConversations(state dashboardState, now time.Time) []ConversationView {
	views := make([]ConversationView, 0, len(state.Sessions))
	for _, session := range state.Sessions {
		views = append(views, buildConversation(session, now))
	}
	return views
}

// ---- the session form ----

// The three values the conversation choice can take. The "@" prefix keeps the
// two sentinels from ever colliding with a stored checkpoint ID, whatever
// scheme the store uses for those.
const (
	conversationKeep = "@keep"
	conversationNew  = "@new"
)

// buildConversationChoices renders the tri-state the backend expects: keep the
// conversation this slot is already in, start a fresh one, or resume a specific
// earlier one. "Keep" is only offered when there is a link to keep, because
// moving a slot to another account or workspace drops it.
func buildConversationChoices(
	current ConversationChoiceContext,
	resumable []model.Session,
	titles map[string]string,
	now time.Time,
) []ChoiceView {
	choices := make([]ChoiceView, 0, len(resumable)+2)
	if current.CanKeep() {
		choices = append(choices, ChoiceView{
			Name:     "conversation",
			Value:    conversationKeep,
			Label:    "Keep this conversation",
			Detail:   strings.TrimPrefix(conversationLabel(current.LinkedID, titles), "Continues "),
			Selected: true,
		})
	}
	choices = append(choices, ChoiceView{
		Name:     "conversation",
		Value:    conversationNew,
		Label:    "Start a new conversation",
		Selected: !current.CanKeep(),
	})
	for _, session := range resumable {
		if session.NativeSessionID == current.LinkedID && current.CanKeep() {
			continue
		}
		choices = append(choices, ChoiceView{
			Name:   "conversation",
			Value:  session.ID,
			Label:  orDefault(truncate(session.Title, 60), "Untitled conversation"),
			Detail: humanSince(session.UpdatedAt, now),
		})
	}
	return choices
}

// ConversationChoiceContext says whether the slot being edited still has a
// conversation link that survives the edit. A move to another account or
// workspace invalidates it, because a conversation belongs where it started.
type ConversationChoiceContext struct {
	LinkedID      string
	SameAccount   bool
	SameWorkspace bool
	IsNewSession  bool
}

func (c ConversationChoiceContext) CanKeep() bool {
	return !c.IsNewSession && c.LinkedID != "" && c.SameAccount && c.SameWorkspace
}

func buildAccountChoices(accounts []model.Account, selectedID string) []ChoiceView {
	usable := authenticatedAccounts(accounts)
	choices := make([]ChoiceView, 0, len(usable))
	for index, account := range usable {
		choices = append(choices, ChoiceView{
			Name:     "accountId",
			Value:    account.ID,
			Label:    account.Email,
			Selected: account.ID == selectedID || (selectedID == "" && index == 0),
		})
	}
	return choices
}

func buildWorkspaceChoices(workspaces []model.Workspace, selectedID string) []ChoiceView {
	choices := make([]ChoiceView, 0, len(workspaces))
	fallback := ""
	for _, workspace := range workspaces {
		if workspace.Selected {
			fallback = workspace.ID
		}
	}
	if fallback == "" && len(workspaces) > 0 {
		fallback = workspaces[0].ID
	}
	for _, workspace := range workspaces {
		choices = append(choices, ChoiceView{
			Name:     "workspaceId",
			Value:    workspace.ID,
			Label:    orDefault(workspace.Label, filepath.Base(workspace.Path)),
			Detail:   shortPath(workspace.Path),
			Selected: workspace.ID == selectedID || (selectedID == "" && workspace.ID == fallback),
		})
	}
	return choices
}

// ---- system health and alerts ----

// buildAlerts reports only what is actually wrong. A healthy system produces an
// empty slice, and the dashboard then says nothing at all — the status area
// exists to carry problems, not to confirm normality.
func buildAlerts(state dashboardState) []AlertView {
	alerts := make([]AlertView, 0, 4)
	health := state.Health
	if !health.TailnetOnly {
		alerts = append(alerts, AlertView{
			Message: "Not reachable over the tailnet",
			Tone:    ToneBad,
			Href:    screenURL("system", ""),
		})
	}
	if !health.CodexHooks {
		message := "Checkpoint hooks need setup"
		if health.CodexHooksInstalled {
			message = "Checkpoint hooks not verified yet"
		}
		alerts = append(alerts, AlertView{Message: message, Tone: ToneWarn, Href: screenURL("system", "")})
	}
	if state.PowerWarning != "" {
		alerts = append(alerts, AlertView{Message: state.PowerWarning, Tone: ToneWarn, Href: screenURL("system", "")})
	}
	for _, remote := range state.Remotes {
		if remote.Worker.LastError != "" {
			alerts = append(alerts, AlertView{
				Message: orDefault(remote.Name, "A session") + " reported a problem",
				Tone:    ToneBad,
				Href:    screenURL("session", remote.ID),
			})
			break
		}
	}
	return alerts
}

func buildSystem(state dashboardState, now time.Time) SystemView {
	health := state.Health
	yesNo := func(ok bool, good, bad string) (string, Tone) {
		if ok {
			return good, ToneGood
		}
		return bad, ToneWarn
	}

	items := make([]HealthItemView, 0, 8)
	add := func(label string, ok bool, good, bad string) {
		detail, tone := yesNo(ok, good, bad)
		items = append(items, HealthItemView{Label: label, Detail: detail, Tone: tone})
	}
	add("Tailscale", health.TailscaleOnline, "Online", "Offline")
	add("Reachable only on the tailnet", health.TailnetOnly, "Yes", "No")
	add("Public sharing (Funnel)", health.FunnelOff, "Off", "On")
	add("Checkpoint hooks", health.CodexHooks, "Verified", hookDetail(health.CodexHooksInstalled))
	add("Power", health.OnACPower, "On mains", "On battery")
	add("Claude Code", health.ClaudeBinary, "Installed", "Not found")
	add("Codex", health.CodexBinary, "Installed", "Not found")
	add("Claude Desktop", health.ClaudeDesktop, "Installed", "Not found")

	return SystemView{
		RetentionDays:  state.RetentionDays,
		Items:          items,
		TailscaleURL:   health.TailscaleURL,
		StartedAgo:     humanSince(state.StartedAt, now),
		OnThisMac:      state.OnThisMac,
		HooksInstalled: health.CodexHooksInstalled,
	}
}

func hookDetail(installed bool) string {
	if installed {
		return "Installed, not verified"
	}
	return "Not installed"
}

// ---- small helpers ----

// userHomeDir caches the home directory. shortPath runs once per rendered path,
// so this avoids a syscall per row on every refresh.
var (
	homeOnce  sync.Once
	homeValue string
	homeErr   error
)

func userHomeDir() (string, error) {
	homeOnce.Do(func() { homeValue, homeErr = os.UserHomeDir() })
	return homeValue, homeErr
}

func indexAccounts(accounts []model.Account) map[string]model.Account {
	index := make(map[string]model.Account, len(accounts))
	for _, account := range accounts {
		index[account.ID] = account
	}
	return index
}

func indexWorkspaces(workspaces []model.Workspace) map[string]model.Workspace {
	index := make(map[string]model.Workspace, len(workspaces))
	for _, workspace := range workspaces {
		index[workspace.ID] = workspace
	}
	return index
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// shortPath collapses the home directory so a long project path still fits a
// phone, matching how errorMessage redacts paths.
func shortPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	if home, err := userHomeDir(); err == nil && home != "" {
		if trimmed == home {
			return "~"
		}
		if strings.HasPrefix(trimmed, home+string(filepath.Separator)) {
			return "~" + trimmed[len(home):]
		}
	}
	return trimmed
}

func truncate(value string, limit int) string {
	trimmed := strings.Join(strings.Fields(value), " ")
	runes := []rune(trimmed)
	if len(runes) <= limit {
		return trimmed
	}
	if limit <= 1 {
		return string(runes[:limit])
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func sentence(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return strings.ToUpper(trimmed[:1]) + trimmed[1:]
}

// humanSince renders a timestamp the way a phone glance wants it. A zero time
// renders empty rather than as a date in 1970.
func humanSince(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	elapsed := now.Sub(at)
	switch {
	case elapsed < 0:
		return "just now"
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return plural(int(elapsed.Minutes()), "minute") + " ago"
	case elapsed < 24*time.Hour:
		return plural(int(elapsed.Hours()), "hour") + " ago"
	case elapsed < 7*24*time.Hour:
		return plural(int(elapsed.Hours()/24), "day") + " ago"
	default:
		return at.Local().Format("2 Jan 2006")
	}
}

func plural(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}

// sortConversations puts pinned conversations first and the most recent first
// within each group, which is the order the list is scanned in.
func sortConversations(views []ConversationView) {
	sort.SliceStable(views, func(first, second int) bool {
		if views[first].Pinned != views[second].Pinned {
			return views[first].Pinned
		}
		return false
	})
}
