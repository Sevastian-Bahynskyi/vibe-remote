package server

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
)

//go:embed templates
var templateFS embed.FS

// screens are the dashboard's addressable pages. Each is a template name, and
// each gets its own pre-bound copy of the layout at startup so a cold page load
// and an htmx swap execute the same screen template rather than two similar ones.
var screens = []string{
	"home",
	"session-new",
	"session",
	"session-edit",
	"accounts",
	"workspaces",
	"conversations",
	"chat",
	"conversation",
	"system",
	"guide",
}

type views struct {
	// base renders fragments: one region or one card, with no page chrome.
	base *template.Template
	// pages renders whole documents, keyed by screen name.
	pages map[string]*template.Template
}

func parseViews() (*views, error) {
	base, err := template.New("views").Funcs(viewFuncs()).ParseFS(templateFS,
		"templates/*.html",
		"templates/screens/*.html",
		"templates/partials/*.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse dashboard templates: %w", err)
	}
	pages := make(map[string]*template.Template, len(screens))
	for _, screen := range screens {
		if base.Lookup(screen) == nil {
			return nil, fmt.Errorf("dashboard screen %q has no template", screen)
		}
		clone, cloneErr := base.Clone()
		if cloneErr != nil {
			return nil, fmt.Errorf("clone dashboard templates: %w", cloneErr)
		}
		// The layout renders {{template "content" .}}; binding it here is what
		// makes the full page and the fragment share one screen definition.
		if _, parseErr := clone.New("content").Parse(fmt.Sprintf("{{template %q .}}", screen)); parseErr != nil {
			return nil, fmt.Errorf("bind screen %q: %w", screen, parseErr)
		}
		pages[screen] = clone
	}
	return &views{base: base, pages: pages}, nil
}

func viewFuncs() template.FuncMap {
	// Deliberately small, and deliberately without any helper that returns
	// template.HTML: one of those and contextual auto-escaping is gone.
	return template.FuncMap{
		"plural": plural,
		"screen": screenURL,
	}
}

// screenURL addresses a screen by query string rather than by path, and that is
// load-bearing rather than cosmetic.
//
// Every URL in the page has to be relative, because Tailscale Serve mounts the
// dashboard at /vibe-remote/ while the service itself answers at /. A relative
// URL resolves against the *directory* of the current document, so a document at
// /ui/sessions/new would resolve "static/styles.css" to /ui/sessions/static/
// styles.css and load with no stylesheet at all. Keeping every document at the
// mount root means relative URLs always resolve against it, whatever the prefix.
//
// Fragment and action URLs stay as paths: they are fetched by htmx and never
// become the document URL, so they are always resolved against the root too.
func screenURL(screen, id string) string {
	if screen == "" || screen == "home" {
		return "?screen=home"
	}
	if id == "" {
		return "?screen=" + url.QueryEscape(screen)
	}
	return "?screen=" + url.QueryEscape(screen) + "&id=" + url.QueryEscape(id)
}

// render writes one template to a buffer first. Executing straight to the
// ResponseWriter would emit half a page under an already-sent 200 if a template
// failed midway.
func (s *Server) render(response http.ResponseWriter, status int, set *template.Template, name string, data any) {
	var buffer bytes.Buffer
	if err := set.ExecuteTemplate(&buffer, name, data); err != nil {
		http.Error(response, "Dashboard unavailable", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(status)
	_, _ = buffer.WriteTo(response)
}

// renderScreen answers a navigation request. htmx asks for the screen alone and
// swaps it into #app; a cold load, a bookmark, or a browser reload gets the
// whole document. Both execute the same screen template.
func (s *Server) renderScreen(response http.ResponseWriter, request *http.Request, status int, screen string, data pageData) {
	if isHTMXRequest(request) {
		// The topbar sits outside #app, and #app is the only region a screen swap
		// replaces, so the topbar has to come back out of band. Without it the
		// title and the back arrow keep describing the screen the user just left.
		s.renderParts(response, status,
			part{screen, data},
			part{"topbar-oob", data},
		)
		return
	}
	set, ok := s.views.pages[screen]
	if !ok {
		http.Error(response, "Dashboard unavailable", http.StatusInternalServerError)
		return
	}
	s.render(response, status, set, "layout", data)
}

// renderFragment answers a partial refresh or an action.
func (s *Server) renderFragment(response http.ResponseWriter, status int, name string, data any) {
	s.render(response, status, s.views.base, name, data)
}

type part struct {
	name string
	data any
}

// renderParts writes several templates as one response body. Every action
// answers with its primary fragment *and* an out-of-band notice: a response
// carrying only the notice would still perform the primary swap, with an empty
// body, and delete the card the user just acted on.
func (s *Server) renderParts(response http.ResponseWriter, status int, parts ...part) {
	var buffer bytes.Buffer
	for _, piece := range parts {
		if err := s.views.base.ExecuteTemplate(&buffer, piece.name, piece.data); err != nil {
			http.Error(response, "Dashboard unavailable", http.StatusInternalServerError)
			return
		}
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(status)
	_, _ = buffer.WriteTo(response)
}

func isHTMXRequest(request *http.Request) bool {
	return request.Header.Get("HX-Request") == "true"
}
