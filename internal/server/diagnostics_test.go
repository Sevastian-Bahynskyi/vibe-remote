package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiagnosticsRequirePrivateHeaderAndExposeOnlyCounters(t *testing.T) {
	server, _ := newTestServerWithStore(t, inertRunner{})
	for _, origin := range []string{"", "https://other.invalid"} {
		request := httptest.NewRequest(http.MethodGet, "/api/diagnostics", nil)
		if origin != "" {
			request.Header.Set("X-Vibe-Remote", "1")
			request.Header.Set("Origin", origin)
		}
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatal("diagnostics boundary accepted", origin, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/diagnostics", nil)
	request.Header.Set("X-Vibe-Remote", "1")
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 6 {
		t.Fatal("unexpected diagnostics fields", len(result))
	}
	for _, key := range []string{"heapAllocBytes", "heapObjects", "goroutines", "workers", "store", "requests"} {
		if result[key] == nil {
			t.Fatal("missing counter", key)
		}
	}
}
