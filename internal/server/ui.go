package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

// pageData is the single shape every screen template receives. One struct with
// optional sections rather than a type per screen: the layout needs the chrome
// fields on every render, and one type means one thing to reason about.
type pageData struct {
	Title  string
	Screen string
	// Back is the relative href of the screen's back arrow, empty on home.
	Back   string
	Notice NoticeView

	Alerts        []AlertView
	Sessions      SessionListView
	Session       SessionDetailView
	Form          SessionFormView
	Accounts      []AccountView
	Workspaces    []WorkspaceView
	Conversations []ConversationView
	Conversation  ConversationDetailView
	System        SystemView
	Counts        CountsView
}

// formLimit mirrors decodeJSON's cap. ParseForm has no size limit of its own for
// urlencoded bodies.
const formLimit = 1 << 20

func parseForm(response http.ResponseWriter, request *http.Request) error {
	request.Body = http.MaxBytesReader(response, request.Body, formLimit)
	return request.ParseForm()
}

// field reads a submitted value. PostFormValue, never FormValue: FormValue also
// searches the URL query, which would let a query parameter spoof a body field.
func field(request *http.Request, name string) string {
	return strings.TrimSpace(request.PostFormValue(name))
}

// parseConversation maps the dashboard's three-way choice onto the tri-state
// resolveResume expects. Note that this reads an empty value as "keep", the
// opposite of the JSON API, where an explicit "" is the deliberate reset: here
// an absent value means the control was never rendered, and keeping the existing
// link is the safe reading. This is the only place that mapping exists.
func parseConversation(value string) *string {
	switch value {
	case conversationKeep, "":
		return nil
	case conversationNew:
		fresh := ""
		return &fresh
	default:
		return &value
	}
}

// ---- screens ----

// dashboard is the only route that serves a document. Which screen it shows
// comes from the query string rather than the path, so the document URL never
// gains a directory level; see screenURL for why that is required rather than
// merely tidy.
func (s *Server) dashboard(response http.ResponseWriter, request *http.Request) {
	id := request.URL.Query().Get("id")
	switch request.URL.Query().Get("screen") {
	case "", "home":
		s.uiHome(response, request)
	case "session-new":
		s.uiSessionNew(response, request)
	case "session":
		s.uiSession(response, request, id)
	case "session-edit":
		s.uiSessionEdit(response, request, id)
	case "accounts":
		s.uiAccounts(response, request)
	case "workspaces":
		s.uiWorkspaces(response, request)
	case "conversations":
		s.uiConversations(response, request)
	case "conversation":
		s.uiConversation(response, request, id)
	case "system":
		s.uiSystem(response, request)
	case "guide":
		s.uiGuide(response, request)
	default:
		// An unknown screen is a stale bookmark, not an error worth a page.
		s.uiHome(response, request)
	}
}

func (s *Server) uiHome(response http.ResponseWriter, request *http.Request) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderScreen(response, request, http.StatusInternalServerError, "home",
			pageData{Screen: "home", Notice: noticeFor(err)})
		return
	}
	s.renderScreen(response, request, http.StatusOK, "home", pageData{
		Screen:   "home",
		Sessions: buildSessionList(state),
		Alerts:   buildAlerts(state),
		Counts: CountsView{
			Accounts:      len(state.Accounts),
			Workspaces:    len(state.Workspaces),
			Conversations: len(state.Sessions),
		},
	})
}

func (s *Server) uiSessionNew(response http.ResponseWriter, request *http.Request) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderScreen(response, request, http.StatusInternalServerError, "session-new",
			pageData{Screen: "session-new", Title: "Start a session", Back: screenURL("home", ""), Notice: noticeFor(err)})
		return
	}
	accountID := preferredAccount(state)
	workspaceID := preferredWorkspace(state)
	list := buildSessionList(state)
	form := SessionFormView{
		Action:      "ui/sessions",
		Title:       "Start a session",
		SubmitLabel: "Start session",
		Accounts:    buildAccountChoices(state.Accounts, accountID),
		Workspaces:  buildWorkspaceChoices(state.Workspaces, workspaceID),
		CanSubmit:   list.CanCreate,
		Blocker:     list.Blocker,
	}
	form.Conversations = s.conversationChoices(request.Context(), state,
		ConversationChoiceContext{IsNewSession: true}, chosen(form.Accounts), chosen(form.Workspaces))
	s.renderScreen(response, request, http.StatusOK, "session-new", pageData{
		Screen: "session-new", Title: "Start a session", Back: screenURL("home", ""), Form: form,
	})
}

func (s *Server) uiSession(response http.ResponseWriter, request *http.Request, id string) {
	detail, err := s.sessionDetail(request.Context(), id, isLocalRequest(request), nil)
	if err != nil {
		s.renderScreen(response, request, statusOf(err), "home",
			pageData{Screen: "home", Notice: noticeFor(err)})
		return
	}
	s.renderScreen(response, request, http.StatusOK, "session", pageData{
		Screen: "session", Title: detail.Session.Name, Back: screenURL("home", ""), Session: detail,
	})
}

func (s *Server) uiSessionEdit(response http.ResponseWriter, request *http.Request, id string) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderScreen(response, request, http.StatusInternalServerError, "home",
			pageData{Screen: "home", Notice: noticeFor(err)})
		return
	}
	remote, found := findRemote(state.Remotes, id)
	if !found {
		s.renderScreen(response, request, http.StatusNotFound, "home",
			pageData{Screen: "home", Notice: NoticeView{Message: "That session no longer exists.", Tone: ToneBad}})
		return
	}
	form := SessionFormView{
		Action:      "ui/sessions/" + id,
		Title:       "Change setup",
		SubmitLabel: "Save and restart",
		SessionID:   id,
		Name:        remote.Name,
		Accounts:    buildAccountChoices(state.Accounts, remote.AccountID),
		Workspaces:  buildWorkspaceChoices(state.Workspaces, remote.WorkspaceID),
		CanSubmit:   true,
	}
	form.Conversations = s.conversationChoices(request.Context(), state, ConversationChoiceContext{
		LinkedID:      remote.ResumeSessionID,
		SameAccount:   chosen(form.Accounts) == remote.AccountID,
		SameWorkspace: chosen(form.Workspaces) == remote.WorkspaceID,
	}, chosen(form.Accounts), chosen(form.Workspaces))
	s.renderScreen(response, request, http.StatusOK, "session-edit", pageData{
		Screen: "session-edit", Title: "Change setup", Back: screenURL("session", id), Form: form,
	})
}

func (s *Server) uiAccounts(response http.ResponseWriter, request *http.Request) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderScreen(response, request, http.StatusInternalServerError, "accounts",
			pageData{Screen: "accounts", Title: "Accounts", Back: screenURL("home", ""), Notice: noticeFor(err)})
		return
	}
	s.renderScreen(response, request, http.StatusOK, "accounts", pageData{
		Screen: "accounts", Title: "Accounts", Back: screenURL("home", ""), Accounts: buildAccounts(state),
	})
}

func (s *Server) uiWorkspaces(response http.ResponseWriter, request *http.Request) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderScreen(response, request, http.StatusInternalServerError, "workspaces",
			pageData{Screen: "workspaces", Title: "Workspaces", Back: screenURL("home", ""), Notice: noticeFor(err)})
		return
	}
	s.renderScreen(response, request, http.StatusOK, "workspaces", pageData{
		Screen: "workspaces", Title: "Workspaces", Back: screenURL("home", ""), Workspaces: buildWorkspaces(state),
	})
}

func (s *Server) uiConversations(response http.ResponseWriter, request *http.Request) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderScreen(response, request, http.StatusInternalServerError, "conversations",
			pageData{Screen: "conversations", Title: "Conversations", Back: screenURL("home", ""), Notice: noticeFor(err)})
		return
	}
	views := buildConversations(state, time.Now())
	sortConversations(views)
	s.renderScreen(response, request, http.StatusOK, "conversations", pageData{
		Screen: "conversations", Title: "Conversations", Back: screenURL("home", ""), Conversations: views,
	})
}

func (s *Server) uiConversation(response http.ResponseWriter, request *http.Request, id string) {
	detail, err := s.conversationDetail(request.Context(), id, isLocalRequest(request))
	if err != nil {
		s.renderScreen(response, request, statusOf(err), "conversations",
			pageData{Screen: "conversations", Title: "Conversations", Back: screenURL("home", ""), Notice: noticeFor(err)})
		return
	}
	s.renderScreen(response, request, http.StatusOK, "conversation", pageData{
		Screen: "conversation", Title: detail.Conversation.Title, Back: screenURL("conversations", ""), Conversation: detail,
	})
}

func (s *Server) uiSystem(response http.ResponseWriter, request *http.Request) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderScreen(response, request, http.StatusInternalServerError, "system",
			pageData{Screen: "system", Title: "System", Back: screenURL("home", ""), Notice: noticeFor(err)})
		return
	}
	s.renderScreen(response, request, http.StatusOK, "system", pageData{
		Screen: "system", Title: "System", Back: screenURL("home", ""), System: buildSystem(state, time.Now()),
	})
}

func (s *Server) uiGuide(response http.ResponseWriter, request *http.Request) {
	s.renderScreen(response, request, http.StatusOK, "guide", pageData{
		Screen: "guide", Title: "How to use", Back: screenURL("home", ""),
	})
}

// ---- read fragments ----

func (s *Server) uiFragmentSessions(response http.ResponseWriter, request *http.Request) {
	state, err := s.slotsSnapshot(request.Context())
	if err != nil {
		s.renderFragment(response, http.StatusInternalServerError, "notice-oob", noticeFor(err))
		return
	}
	state.OnThisMac = isLocalRequest(request)
	state.Health = s.health(request.Context())
	s.renderFragment(response, http.StatusOK, "session-list", buildSessionList(state))
}

func (s *Server) uiFragmentAlerts(response http.ResponseWriter, request *http.Request) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderFragment(response, http.StatusInternalServerError, "notice-oob", noticeFor(err))
		return
	}
	s.renderFragment(response, http.StatusOK, "alerts", buildAlerts(state))
}

func (s *Server) uiFragmentSession(response http.ResponseWriter, request *http.Request) {
	detail, err := s.sessionDetail(request.Context(), request.PathValue("id"), isLocalRequest(request), nil)
	if err != nil {
		s.renderFragment(response, statusOf(err), "notice-oob", noticeFor(err))
		return
	}
	s.renderFragment(response, http.StatusOK, "session-detail", detail)
}

// uiFragmentConversationOptions repopulates the conversation choices when the
// account or workspace selection changes, because a conversation belongs to the
// account and workspace it started in.
func (s *Server) uiFragmentConversationOptions(response http.ResponseWriter, request *http.Request) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderFragment(response, http.StatusInternalServerError, "notice-oob", noticeFor(err))
		return
	}
	query := request.URL.Query()
	accountID := query.Get("accountId")
	workspaceID := query.Get("workspaceId")
	context := ConversationChoiceContext{IsNewSession: true}
	if sessionID := query.Get("sessionId"); sessionID != "" {
		if remote, found := findRemote(state.Remotes, sessionID); found {
			context = ConversationChoiceContext{
				LinkedID:      remote.ResumeSessionID,
				SameAccount:   accountID == remote.AccountID,
				SameWorkspace: workspaceID == remote.WorkspaceID,
			}
		}
	}
	choices := s.conversationChoices(request.Context(), state, context, accountID, workspaceID)
	s.renderFragment(response, http.StatusOK, "conversation-choices", choices)
}

// ---- session actions ----

func (s *Server) uiCreateSession(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
		return
	}
	body := remoteSessionRequest{
		Name:            field(request, "name"),
		AccountID:       field(request, "accountId"),
		WorkspaceID:     field(request, "workspaceId"),
		ResumeSessionID: parseConversation(field(request, "conversation")),
	}
	remote, err := s.createSlotOp(request.Context(), body)
	if err != nil {
		s.renderFragment(response, statusOf(err), "notice-oob", noticeFor(err))
		return
	}
	// A created session has its own screen worth landing on: the user's next
	// move is almost always to open it in Claude.
	response.Header().Set("HX-Push-Url", screenURL("session", remote.ID))
	s.respondWithSession(response, request, http.StatusCreated, remote.ID,
		NoticeView{Message: remote.Name + " is ready.", Tone: ToneGood}, nil)
}

func (s *Server) uiUpdateSession(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
		return
	}
	id := request.PathValue("id")
	body := remoteSessionRequest{
		Name:            field(request, "name"),
		AccountID:       field(request, "accountId"),
		WorkspaceID:     field(request, "workspaceId"),
		ResumeSessionID: parseConversation(field(request, "conversation")),
		Force:           field(request, "force") == "1",
	}
	remote, err := s.updateSlotOp(request.Context(), id, body)
	if err != nil {
		s.failSessionAction(response, request, id, err, "ui/sessions/"+id, "Save anyway", formFields(request))
		return
	}
	response.Header().Set("HX-Push-Url", screenURL("session", remote.ID))
	s.respondWithSession(response, request, http.StatusOK, remote.ID,
		NoticeView{Message: remote.Name + " restarted with the new settings.", Tone: ToneGood}, nil)
}

func (s *Server) uiSessionLifecycle(action string) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if err := parseForm(response, request); err != nil {
			s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
			return
		}
		id := request.PathValue("id")
		force := field(request, "force") == "1"

		var remote model.RemoteSession
		var err error
		var message, confirmLabel string
		switch action {
		case "start":
			remote, err = s.lifecycleSlotOp(request.Context(), id, false, force)
			message, confirmLabel = " started.", "Start anyway"
		case "restart":
			remote, err = s.lifecycleSlotOp(request.Context(), id, true, force)
			message, confirmLabel = " restarted.", "Restart anyway"
		case "stop":
			remote, err = s.stopSlotOp(request.Context(), id, force)
			message, confirmLabel = " stopped. Its conversations are kept.", "Stop anyway"
		}
		if err != nil {
			s.failSessionAction(response, request, id, err, "ui/sessions/"+id+"/"+action, confirmLabel, nil)
			return
		}
		s.respondWithSession(response, request, http.StatusOK, id,
			NoticeView{Message: remote.Name + message, Tone: ToneGood}, nil)
	}
}

func (s *Server) uiOpenDesktop(response http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	remote, err := s.openDesktopOp(request.Context(), id, isLocalRequest(request))
	if err != nil {
		s.renderSessionOutcome(response, request, statusOf(err), id, noticeFor(err), nil)
		return
	}
	s.respondWithSession(response, request, http.StatusOK, id,
		NoticeView{Message: remote.Name + " opened in Claude Desktop.", Tone: ToneGood}, nil)
}

// uiDeleteSession confirms in place before acting. The first submission renders
// the confirmation row; the second carries confirmed=1 and does the work.
func (s *Server) uiDeleteSession(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
		return
	}
	id := request.PathValue("id")
	if field(request, "confirmed") != "1" {
		s.renderSessionOutcome(response, request, http.StatusOK, id, NoticeView{}, &ConfirmView{
			Post:    "ui/sessions/" + id + "/delete",
			Label:   "Delete session",
			Message: "This stops the session and removes it. Its conversations are kept.",
			Cancel:  "ui/fragments/session/" + id,
			Fields:  []FieldView{{Name: "confirmed", Value: "1"}},
			Danger:  true,
		})
		return
	}
	remote, err := s.deleteSlotOp(request.Context(), id)
	if err != nil {
		s.renderSessionOutcome(response, request, statusOf(err), id, noticeFor(err), nil)
		return
	}
	// The session is gone, so its screen is gone with it.
	response.Header().Set("HX-Location", screenURL("home", ""))
	s.renderFragment(response, http.StatusOK, "notice-oob",
		NoticeView{Message: remote.Name + " deleted.", Tone: ToneGood})
}

// ---- account, workspace, conversation and system actions ----

func (s *Server) uiAddAccount(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
		return
	}
	notice := NoticeView{Message: "Sign-in opened on the Mac. The account appears once it completes.", Tone: ToneGood}
	if err := s.addAccountOp(field(request, "email")); err != nil {
		s.renderAccounts(response, request, statusOf(err), noticeFor(err))
		return
	}
	s.renderAccounts(response, request, http.StatusOK, notice)
}

func (s *Server) uiRefreshAccount(response http.ResponseWriter, request *http.Request) {
	account, reopened, err := s.refreshAccountOp(request.Context(), request.PathValue("id"))
	if err != nil {
		s.renderAccounts(response, request, statusOf(err), noticeFor(err))
		return
	}
	notice := NoticeView{Message: "Sign-in verified for " + account.Email + ".", Tone: ToneGood}
	if reopened {
		notice = NoticeView{Message: "Sign-in reopened on the Mac.", Tone: ToneWarn}
	}
	s.renderAccounts(response, request, http.StatusOK, notice)
}

func (s *Server) uiDeleteAccount(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
		return
	}
	id := request.PathValue("id")
	deleteConversations := field(request, "deleteConversations") == "1"
	if err := s.removeAccountOp(request.Context(), id, deleteConversations); err != nil {
		s.renderAccounts(response, request, statusOf(err), noticeFor(err))
		return
	}
	s.renderAccounts(response, request, http.StatusOK,
		NoticeView{Message: "Account removed.", Tone: ToneGood})
}

func (s *Server) uiAddWorkspace(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
		return
	}
	workspace, err := s.addWorkspaceOp(request.Context(), field(request, "label"), field(request, "path"))
	if err != nil {
		s.renderWorkspaces(response, request, statusOf(err), noticeFor(err))
		return
	}
	s.renderWorkspaces(response, request, http.StatusOK,
		NoticeView{Message: orDefault(workspace.Label, workspace.Path) + " added.", Tone: ToneGood})
}

func (s *Server) uiDeleteWorkspace(response http.ResponseWriter, request *http.Request) {
	if err := s.removeWorkspaceOp(request.Context(), request.PathValue("id")); err != nil {
		s.renderWorkspaces(response, request, statusOf(err), noticeFor(err))
		return
	}
	s.renderWorkspaces(response, request, http.StatusOK,
		NoticeView{Message: "Workspace removed. Your files were not touched.", Tone: ToneGood})
}

func (s *Server) uiUpdateConversation(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
		return
	}
	id := request.PathValue("id")
	session, err := s.store.GetSession(request.Context(), id)
	if err != nil {
		s.renderFragment(response, http.StatusNotFound, "notice-oob",
			NoticeView{Message: "That conversation no longer exists.", Tone: ToneBad})
		return
	}
	title := session.Title
	if submitted := field(request, "title"); submitted != "" {
		title = submitted
	}
	pinned := session.Pinned
	if action := field(request, "pinned"); action != "" {
		pinned = action == "1"
	}
	if _, err := s.updateSessionOp(request.Context(), id, title, pinned); err != nil {
		s.renderConversation(response, request, statusOf(err), id, noticeFor(err))
		return
	}
	s.renderConversation(response, request, http.StatusOK, id,
		NoticeView{Message: "Conversation updated.", Tone: ToneGood})
}

func (s *Server) uiCreateHandoff(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
		return
	}
	id := field(request, "sourceSessionId")
	handoff, destination, err := s.createHandoffOp(request.Context(), id,
		model.Provider(field(request, "destinationProvider")), field(request, "destinationAccountId"))
	if err != nil {
		s.renderConversation(response, request, statusOf(err), id, noticeFor(err))
		return
	}
	_ = handoff
	s.renderConversation(response, request, http.StatusOK, id, NoticeView{
		Message: handoffInstructions(handoff.DestinationProvider, destination),
		Tone:    ToneGood,
	})
}

func (s *Server) uiInstallHooks(response http.ResponseWriter, request *http.Request) {
	if err := s.installHooksOp(request.Context()); err != nil {
		s.renderSystem(response, request, statusOf(err), noticeFor(err))
		return
	}
	s.renderSystem(response, request, http.StatusOK,
		NoticeView{Message: "Checkpoint hooks reinstalled.", Tone: ToneGood})
}

// ---- response helpers ----

// noticeFor renders an operation error through exactly the same redaction the
// JSON API applies, so neither surface can start disclosing more than the other.
func noticeFor(err error) NoticeView {
	return NoticeView{Message: errorMessage(err, statusOf(err)), Tone: ToneBad}
}

// respondWithSession answers a session action. Home sends from=home and gets the
// refreshed list; the session screen gets its own region back.
func (s *Server) respondWithSession(
	response http.ResponseWriter, request *http.Request,
	status int, id string, notice NoticeView, confirm *ConfirmView,
) {
	s.renderSessionOutcome(response, request, status, id, notice, confirm)
}

func (s *Server) renderSessionOutcome(
	response http.ResponseWriter, request *http.Request,
	status int, id string, notice NoticeView, confirm *ConfirmView,
) {
	if request.PostFormValue("from") == "home" && confirm == nil {
		state, err := s.slotsSnapshot(request.Context())
		if err != nil {
			s.renderFragment(response, http.StatusInternalServerError, "notice-oob", noticeFor(err))
			return
		}
		state.OnThisMac = isLocalRequest(request)
		state.Health = s.health(request.Context())
		s.renderParts(response, status,
			part{"session-list", buildSessionList(state)},
			part{"notice-oob", notice},
		)
		return
	}
	detail, err := s.sessionDetail(request.Context(), id, isLocalRequest(request), confirm)
	if err != nil {
		s.renderFragment(response, statusOf(err), "notice-oob", noticeFor(err))
		return
	}
	s.renderParts(response, status,
		part{"session-detail", detail},
		part{"notice-oob", notice},
	)
}

// failSessionAction turns a refused action into either an inline confirmation or
// a plain error. A forcible error means Claude is mid-turn: the same action is
// offered again with force set, replaying the original fields so the confirmed
// action is the same action.
func (s *Server) failSessionAction(
	response http.ResponseWriter, request *http.Request,
	id string, err error, post, label string, fields []FieldView,
) {
	if !forcible(err) {
		s.renderSessionOutcome(response, request, statusOf(err), id, noticeFor(err), nil)
		return
	}
	s.renderSessionOutcome(response, request, statusOf(err), id, NoticeView{}, &ConfirmView{
		Post:    post,
		Label:   label,
		Message: errorMessage(err, statusOf(err)) + " Forcing it ends the running turn.",
		Cancel:  "ui/fragments/session/" + id,
		Fields:  append(fields, FieldView{Name: "force", Value: "1"}),
	})
}

// formFields replays a submission as hidden inputs so a forced retry carries the
// same choices the user already made.
func formFields(request *http.Request) []FieldView {
	names := []string{"name", "accountId", "workspaceId", "conversation"}
	fields := make([]FieldView, 0, len(names))
	for _, name := range names {
		if value := request.PostFormValue(name); value != "" {
			fields = append(fields, FieldView{Name: name, Value: value})
		}
	}
	return fields
}

func (s *Server) renderAccounts(response http.ResponseWriter, request *http.Request, status int, notice NoticeView) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderFragment(response, http.StatusInternalServerError, "notice-oob", noticeFor(err))
		return
	}
	s.renderParts(response, status,
		part{"account-list", buildAccounts(state)},
		part{"notice-oob", notice},
	)
}

func (s *Server) renderWorkspaces(response http.ResponseWriter, request *http.Request, status int, notice NoticeView) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderFragment(response, http.StatusInternalServerError, "notice-oob", noticeFor(err))
		return
	}
	s.renderParts(response, status,
		part{"workspace-list", buildWorkspaces(state)},
		part{"notice-oob", notice},
	)
}

func (s *Server) renderConversation(response http.ResponseWriter, request *http.Request, status int, id string, notice NoticeView) {
	detail, err := s.conversationDetail(request.Context(), id, isLocalRequest(request))
	if err != nil {
		s.renderFragment(response, statusOf(err), "notice-oob", noticeFor(err))
		return
	}
	s.renderParts(response, status,
		part{"conversation-detail", detail},
		part{"notice-oob", notice},
	)
}

func (s *Server) renderSystem(response http.ResponseWriter, request *http.Request, status int, notice NoticeView) {
	state, err := s.snapshot(request.Context(), isLocalRequest(request))
	if err != nil {
		s.renderFragment(response, http.StatusInternalServerError, "notice-oob", noticeFor(err))
		return
	}
	s.renderParts(response, status,
		part{"system-panel", buildSystem(state, time.Now())},
		part{"notice-oob", notice},
	)
}

// ---- shared lookups ----

func (s *Server) sessionDetail(ctx context.Context, id string, local bool, confirm *ConfirmView) (SessionDetailView, error) {
	state, err := s.slotsSnapshot(ctx)
	if err != nil {
		return SessionDetailView{}, fail(http.StatusInternalServerError, err)
	}
	state.OnThisMac = local
	state.Health = s.health(ctx)
	remote, found := findRemote(state.Remotes, id)
	if !found {
		return SessionDetailView{}, failText(http.StatusNotFound, "that session no longer exists")
	}
	return SessionDetailView{Session: buildSession(state, remote), Confirm: confirm}, nil
}

func (s *Server) conversationDetail(ctx context.Context, id string, local bool) (ConversationDetailView, error) {
	session, err := s.store.GetSession(ctx, id)
	if err != nil {
		return ConversationDetailView{}, failText(http.StatusNotFound, "that conversation no longer exists")
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return ConversationDetailView{}, fail(http.StatusInternalServerError, err)
	}
	sessions := []model.Session{session}
	decorateSessions(sessions, accounts)
	eligible := make([]model.Account, 0, len(accounts))
	for _, account := range accounts {
		if session.Provider != model.ProviderClaude || account.ID != session.AccountID {
			eligible = append(eligible, account)
		}
	}
	return ConversationDetailView{
		Conversation: buildConversation(sessions[0], time.Now()),
		Destinations: buildHandoffDestinations(eligible),
	}, nil
}

// conversationChoices lists the conversations that could be resumed for the
// chosen account and workspace. Only those are read, rather than shipping every
// stored conversation to the browser to filter there.
func (s *Server) conversationChoices(
	ctx context.Context, state dashboardState,
	choiceContext ConversationChoiceContext, accountID, workspaceID string,
) []ChoiceView {
	workspacePath := ""
	for _, workspace := range state.Workspaces {
		if workspace.ID == workspaceID {
			workspacePath = workspace.Path
		}
	}
	var resumable []model.Session
	if accountID != "" && workspacePath != "" {
		found, err := s.store.ListSessions(ctx, store.SessionFilter{
			Provider: model.ProviderClaude, AccountID: accountID, WorkspacePath: workspacePath, Limit: 40,
		})
		if err == nil {
			resumable = found
		}
	}
	return buildConversationChoices(choiceContext, resumable, state.ConversationTitles, time.Now())
}

func findRemote(remotes []model.RemoteSession, id string) (model.RemoteSession, bool) {
	for _, remote := range remotes {
		if remote.ID == id {
			return remote, true
		}
	}
	return model.RemoteSession{}, false
}

// preferredAccount defaults the form to the account of the most recently created
// session, so the common case is two taps.
func preferredAccount(state dashboardState) string {
	newest := time.Time{}
	preferred := ""
	for _, remote := range state.Remotes {
		if remote.CreatedAt.After(newest) {
			newest = remote.CreatedAt
			preferred = remote.AccountID
		}
	}
	return preferred
}

func preferredWorkspace(state dashboardState) string {
	for _, workspace := range state.Workspaces {
		if workspace.Selected {
			return workspace.ID
		}
	}
	return ""
}

func chosen(choices []ChoiceView) string {
	for _, choice := range choices {
		if choice.Selected {
			return choice.Value
		}
	}
	return ""
}
