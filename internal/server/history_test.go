package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHistoryCacheMissRetrievesCompleteDashboard(t *testing.T) {
	s, _ := newTestServerWithStore(t, inertRunner{})
	request := httptest.NewRequest(http.MethodGet, "/?screen=accounts", nil)
	request.Header.Set("HX-Request", "true")
	request.Header.Set("HX-History-Restore-Request", "true")
	response := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(response, request)
	for _, text := range []string{"<!doctype html>", `id="app"`, `id="topbar"`, `"historyCacheSize":0`, `hx-history="false"`} {
		if !strings.Contains(response.Body.String(), text) {
			t.Fatalf("history response omitted %s", text)
		}
	}
}
