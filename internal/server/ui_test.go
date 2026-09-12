package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

// get issues a dashboard GET. htmx sets HX-Request on every request it makes,
// so the flag decides whether a screen answers with a fragment or a document.
func get(t *testing.T, service *Server, target string, htmx bool) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if htmx {
		request.Header.Set("HX-Request", "true")
	}
	response := httptest.NewRecorder()
	service.http.Handler.ServeHTTP(response, request)
	return response.Code, response.Body.String()
}

func post(t *testing.T, service *Server, target string, form map[string]string) (int, string) {
	t.Helper()
	values := make([]string, 0, len(form))
	for name, value := range form {
		values = append(values, name+"="+value)
	}
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(strings.Join(values, "&")))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Vibe-Remote", "1")
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	service.http.Handler.ServeHTTP(response, request)
	return response.Code, response.Body.String()
}

// Templates are parsed in New, so any screen that lost its definition fails the
// binary at startup rather than one request on a phone.
func TestEveryScreenHasATemplate(t *testing.T) {
	t.Parallel()
	parsed, err := parseViews()
	if err != nil {
		t.Fatal(err)
	}
	for _, screen := range screens {
		if parsed.pages[screen] == nil {
			t.Errorf("screen %q has no page template", screen)
		}
	}
}

// Tailscale Serve mounts the dashboard at /vibe-remote/, so every URL the
// markup references has to be relative. One leading slash and the whole page
// starts talking to the wrong origin path.
func TestRenderedMarkupUsesRelativeURLs(t *testing.T) {
	t.Parallel()
	service := newTestServer(t)
	absolute := regexp.MustCompile(`(?:hx-(?:get|post)|href|src|action)="/[^/]`)

	for _, target := range []string{"/", "/?screen=accounts", "/?screen=workspaces", "/?screen=conversations", "/?screen=system", "/?screen=guide", "/?screen=session-new"} {
		status, body := get(t, service, target, false)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d", target, status)
		}
		if found := absolute.FindAllString(body, -1); found != nil {
			t.Errorf("GET %s emits absolute URLs, which break the /vibe-remote/ mount: %v", target, found)
		}
	}
}

// Regression guard. Screens are addressed by query string precisely so that the
// document URL never gains a directory level: a document at /ui/sessions/new
// resolves the relative "static/styles.css" against /ui/sessions/ and loads with
// no stylesheet at all. Any navigation URL containing a slash reintroduces that.
func TestNavigationNeverDeepensTheDocumentURL(t *testing.T) {
	t.Parallel()
	service, database := newTestServerWithStore(t, inertRunner{})
	remote := seedSession(t, database)

	// hx-push-url and hx-target="#app" together mark a URL that becomes the
	// document URL; those are the ones that must stay at the mount root.
	navigation := regexp.MustCompile(`hx-get="([^"]+)"[^>]*hx-target="#app"`)
	for _, target := range []string{"/", "/?screen=accounts", "/?screen=conversations", "/?screen=session&id=" + remote.ID} {
		_, body := get(t, service, target, false)
		for _, match := range navigation.FindAllStringSubmatch(body, -1) {
			if strings.Contains(match[1], "/") {
				t.Errorf("GET %s: navigating to %q adds a path level, which breaks relative asset URLs", target, match[1])
			}
		}
	}

	if got := screenURL("session", "rs_1"); strings.Contains(got, "/") {
		t.Errorf("screenURL = %q, want a query-only URL", got)
	}
}

// The CSP has no unsafe-inline and no unsafe-eval. A violation is invisible to
// Go tests unless something looks for it, so this looks for it.
func TestRenderedMarkupSurvivesTheContentSecurityPolicy(t *testing.T) {
	t.Parallel()
	service := newTestServer(t)
	_, body := get(t, service, "/", false)

	if strings.Contains(body, "hx-on:") || strings.Contains(body, `"js:`) || strings.Contains(body, "hx-vals='js:") {
		t.Error("hx-on / hx-vals js: compile strings with the Function constructor, which unsafe-eval would be needed for")
	}
	if regexp.MustCompile(`\sstyle="`).MatchString(body) {
		t.Error("inline style attribute is blocked by style-src 'self'")
	}
	// Every <script> must be a src reference with an empty body.
	for _, tag := range regexp.MustCompile(`(?s)<script\b[^>]*>(.*?)</script>`).FindAllStringSubmatch(body, -1) {
		if strings.TrimSpace(tag[1]) != "" {
			t.Errorf("inline script is blocked by script-src 'self': %q", strings.TrimSpace(tag[1]))
		}
	}
	if !strings.Contains(body, `"includeIndicatorStyles":false`) {
		t.Error("htmx injects a <style> element unless includeIndicatorStyles is disabled, and style-src 'self' blocks it")
	}
	// A 4xx body must still be swapped, or the inline force-confirm row never renders.
	if !strings.Contains(body, `{"code":"[45]..","swap":true`) {
		t.Error("htmx drops non-2xx bodies unless responseHandling says otherwise")
	}
}

// A cold page load and an htmx swap must produce the same screen, or the two
// drift apart silently.
func TestScreenRendersIdenticallyAsFragmentAndPage(t *testing.T) {
	t.Parallel()
	service := newTestServer(t)

	for _, target := range []string{"/", "/?screen=accounts", "/?screen=system", "/?screen=guide"} {
		_, page := get(t, service, target, false)
		_, fragment := get(t, service, target, true)
		if strings.TrimSpace(fragment) == "" {
			t.Fatalf("GET %s returned an empty fragment", target)
		}
		if strings.Contains(fragment, "<!doctype html>") {
			t.Errorf("GET %s returned a whole document to an htmx request", target)
		}
		// An htmx screen response is the screen followed by the topbar as an
		// out-of-band swap. The page renders that same topbar inline instead, so
		// the two halves are checked separately: the screen markup must be
		// byte-identical to the page's, and the topbar must be there to swap.
		screen, topbar, split := strings.Cut(fragment, `<header id="topbar"`)
		if !split {
			t.Errorf("GET %s: htmx response carries no out-of-band topbar, so the title and back arrow would go stale", target)
			continue
		}
		if !strings.Contains(topbar, `hx-swap-oob="true"`) {
			t.Errorf("GET %s: the topbar is present but not marked for an out-of-band swap", target)
		}
		if !strings.Contains(page, strings.TrimSpace(screen)) {
			t.Errorf("GET %s: the page and the fragment render different markup", target)
		}
	}
}

func TestDashboardMutationsRequireTheDashboardHeader(t *testing.T) {
	t.Parallel()
	service := newTestServer(t)
	request := httptest.NewRequest(http.MethodPost, "/ui/workspaces", strings.NewReader("label=x&path=/tmp"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	service.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("unguarded POST /ui/workspaces = %d, want 403", response.Code)
	}
}

// html/template's contextual escaping is the only thing standing between a
// hostile account label and the dashboard. There is deliberately no FuncMap
// helper returning template.HTML; this fails the moment someone adds one.
func TestUntrustedTextIsEscaped(t *testing.T) {
	t.Parallel()
	service, database := newTestServerWithStore(t, inertRunner{})
	if _, err := database.UpsertWorkspace(context.Background(), model.Workspace{
		Label: `<img src=x onerror=alert(1)>`, Path: t.TempDir(), Selected: true,
	}); err != nil {
		t.Fatal(err)
	}

	status, body := get(t, service, "/?screen=workspaces", false)
	if status != http.StatusOK {
		t.Fatalf("GET /?screen=workspaces = %d", status)
	}
	if strings.Contains(body, "<img src=x") {
		t.Fatal("a workspace label reached the page unescaped")
	}
	if !strings.Contains(body, "&lt;img") {
		t.Fatal("expected the label to render escaped")
	}
}

// The conversation tri-state is the semantic most easily lost in a rewrite:
// omitted keeps the slot's conversation, "" starts a fresh one, an id resumes
// that one. The form encoding has to land on exactly those three.
func TestParseConversationCoversTheTriState(t *testing.T) {
	t.Parallel()
	if got := parseConversation(conversationKeep); got != nil {
		t.Fatalf("keep = %v, want nil so the linked conversation survives", got)
	}
	// An absent control means it was never rendered; keeping the link is the
	// safe reading, and is the opposite of what the JSON API makes of "".
	if got := parseConversation(""); got != nil {
		t.Fatalf("absent = %v, want nil", got)
	}
	fresh := parseConversation(conversationNew)
	if fresh == nil || *fresh != "" {
		t.Fatalf("new = %v, want a pointer to the empty string", fresh)
	}
	resume := parseConversation("ses_123")
	if resume == nil || *resume != "ses_123" {
		t.Fatalf("resume = %v, want a pointer to the id", resume)
	}
}

// The sentinels must never be mistakable for a stored identifier.
func TestConversationSentinelsCannotCollideWithAnIdentifier(t *testing.T) {
	t.Parallel()
	for _, sentinel := range []string{conversationKeep, conversationNew} {
		if !strings.HasPrefix(sentinel, "@") {
			t.Fatalf("sentinel %q must be prefixed so it cannot collide with a store id", sentinel)
		}
		if model.ValidResumeSessionID(strings.TrimPrefix(sentinel, "@")) && strings.Contains(sentinel, "@") == false {
			t.Fatalf("sentinel %q is shaped like a resumable id", sentinel)
		}
	}
}

// A destructive action confirms in place rather than in a modal, and does
// nothing until the confirmation comes back.
func TestDeletingASessionAsksFirst(t *testing.T) {
	t.Parallel()
	service, database := newTestServerWithStore(t, inertRunner{})
	remote := seedSession(t, database)

	status, body := post(t, service, "/ui/sessions/"+remote.ID+"/delete", map[string]string{})
	if status != http.StatusOK {
		t.Fatalf("first delete = %d, want 200 with a confirmation", status)
	}
	if !strings.Contains(body, `data-testid="confirm"`) {
		t.Fatal("first delete did not ask for confirmation")
	}
	if !strings.Contains(body, `name="confirmed" value="1"`) {
		t.Fatal("the confirmation must replay the action with its confirmed flag")
	}
	if _, err := database.GetRemoteSession(context.Background(), remote.ID); err != nil {
		t.Fatal("the session was deleted before it was confirmed")
	}

	status, _ = post(t, service, "/ui/sessions/"+remote.ID+"/delete", map[string]string{"confirmed": "1"})
	if status != http.StatusOK {
		t.Fatalf("confirmed delete = %d", status)
	}
	if _, err := database.GetRemoteSession(context.Background(), remote.ID); err == nil {
		t.Fatal("the confirmed delete did not remove the session")
	}
}

// Every action answers with its primary fragment as well as the notice. A
// response carrying only the out-of-band notice would still perform the primary
// swap, with an empty body, and delete the region the user just acted on.
func TestActionResponsesCarryBothTheRegionAndTheNotice(t *testing.T) {
	t.Parallel()
	service, database := newTestServerWithStore(t, inertRunner{})
	seedSession(t, database)

	status, body := post(t, service, "/ui/workspaces", map[string]string{"label": "Added", "path": t.TempDir()})
	if status != http.StatusOK {
		t.Fatalf("POST /ui/workspaces = %d: %s", status, body)
	}
	if !strings.Contains(body, `id="workspace-list"`) {
		t.Fatal("the action did not return its primary region, so the swap would blank it")
	}
	if !strings.Contains(body, `id="notice"`) || !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Fatal("the action did not return the out-of-band notice")
	}
}

// Polled regions carry their own id and refresh attributes so that swapping one
// in re-installs its own behaviour.
func TestPolledRegionsReinstallThemselves(t *testing.T) {
	t.Parallel()
	service := newTestServer(t)
	status, body := get(t, service, "/ui/fragments/sessions", true)
	if status != http.StatusOK {
		t.Fatalf("GET /ui/fragments/sessions = %d", status)
	}
	if !strings.Contains(body, `id="session-list"`) || !strings.Contains(body, `data-refresh="ui/fragments/sessions"`) {
		t.Fatalf("the refreshed region did not carry its own id and refresh target: %s", body)
	}
}

func seedSession(t *testing.T, database *store.Store) model.RemoteSession {
	t.Helper()
	ctx := context.Background()
	account, err := database.UpsertAccount(ctx, model.Account{
		Email: "person@example.com", ProfileDir: t.TempDir(), Status: model.AccountAuthenticated,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := database.UpsertWorkspace(ctx, model.Workspace{
		Label: "Project", Path: t.TempDir(), Selected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := database.UpsertRemoteSession(ctx, model.RemoteSession{
		Name: "Tablet", AccountID: account.ID, WorkspaceID: workspace.ID,
		WorkspacePath: workspace.Path, Desired: model.DesiredStopped,
	})
	if err != nil {
		t.Fatal(err)
	}
	return remote
}

// Deleting a session used to answer with an HX-Location header. htmx handles
// that by re-fetching the screen and, with no target given, swapping it into
// <body> — which replaced the topbar, the notice host and #app itself with a
// bare screen fragment. The result was a headerless page with no targets left
// for any later swap, and the confirmation notice was dropped too, because htmx
// returns from that header before it reads the body.
func TestDeletingASessionLandsOnAWholeHomeScreen(t *testing.T) {
	t.Parallel()
	service, database := newTestServerWithStore(t, inertRunner{})
	seedRunnableSlot(t, service, database, "rs_doomed", "dddddddd-1111-4111-8111-111111111111")

	request := httptest.NewRequest(http.MethodPost, "/ui/sessions/rs_doomed/delete",
		strings.NewReader("confirmed=1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Vibe-Remote", "1")
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	service.http.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("HX-Location"); got != "" {
		t.Errorf("HX-Location = %q; it swaps <body> and discards this body", got)
	}
	if got := response.Header().Get("HX-Retarget"); got != "#app" {
		t.Errorf("HX-Retarget = %q, want #app so the page chrome survives", got)
	}
	if got := response.Header().Get("HX-Reswap"); got != "innerHTML" {
		t.Errorf("HX-Reswap = %q, want innerHTML", got)
	}
	if got := response.Header().Get("HX-Push-Url"); got != screenURL("home", "") {
		t.Errorf("HX-Push-Url = %q, want the home screen", got)
	}

	body := response.Body.String()
	// The home screen itself...
	if !strings.Contains(body, "Start a session") {
		t.Errorf("body does not render the home screen: %s", body)
	}
	// ...a fresh topbar, since the old one still said the deleted session's
	// name and carried a back arrow to a screen that no longer exists...
	if !strings.Contains(body, `<header id="topbar"`) || !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Errorf("body does not refresh the topbar out of band: %s", body)
	}
	if strings.Contains(body, "topbar-back") {
		t.Errorf("home topbar still carries a back arrow: %s", body)
	}
	// ...and the confirmation the user acted on something.
	if !strings.Contains(body, "deleted.") {
		t.Errorf("body carries no deletion notice: %s", body)
	}

	if _, err := database.GetRemoteSession(context.Background(), "rs_doomed"); err == nil {
		t.Error("session still exists after delete")
	}
}
