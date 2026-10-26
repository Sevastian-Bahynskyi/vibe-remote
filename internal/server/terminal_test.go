package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
)

func TestConsoleRejectsCrossOriginAndUnvalidatedInput(t *testing.T) {
	t.Parallel()
	s := newTestServer(t)
	for _, test := range []struct {
		method, body, origin, site, header string
		want                               int
	}{
		{method: "GET", want: 403},
		{method: "GET", header: "1", origin: "https://other.example", want: 403},
		{method: "GET", header: "1", site: "cross-site", want: 403},
		{method: "POST", header: "1", body: `{"runId":"run","key":"text","data":"open Notes"}`, want: 400},
		{method: "POST", header: "1", body: `{"runId":"run","key":"enter","rows":30}`, want: 400},
		{method: "POST", header: "1", body: `{"runId":"run","columns":1,"rows":1}`, want: 400},
		{method: "POST", header: "1", body: `{"runId":"run","key":"enter"}{}`, want: 400},
	} {
		r := httptest.NewRequest(test.method, "http://localhost/api/remote-sessions/slot/console", strings.NewReader(test.body))
		r.Header.Set("X-Vibe-Remote", test.header)
		r.Header.Set("Origin", test.origin)
		r.Header.Set("Sec-Fetch-Site", test.site)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != test.want {
			t.Errorf("console %s = %d, want %d", test.method, w.Code, test.want)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Error("console output can be cached")
		}
	}
}

func TestConsoleControlsTheSelectedPTYThroughMountedDashboard(t *testing.T) {
	t.Parallel()
	s, database := newTestServerWithStore(t, inertRunner{})
	script := filepath.Join(t.TempDir(), "claude-test")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = auth ]; then
  printf '{"loggedIn":true}\n'
  exit 0
fi
stty -echo -icanon
printf 'https://claude.ai/code/test-console\r\nAllow Notes for this session?\r\n'
while :; do
  dd bs=1 count=1 of=/dev/null 2>/dev/null || exit
  printf '\r\nKEY_RECEIVED\r\n'
done
`), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := claude.New(s.layout.Root, database, claude.Options{BaselineSource: "-", ClaudePath: script, StopTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	s.claude = m
	seedRunnableSlot(t, s, database, "console", "11111111-2222-3333-4444-555555555555")
	remote, err := database.GetRemoteSession(t.Context(), "console")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(t.Context(), workerSpec(remote)); err != nil {
		t.Fatal(err)
	}
	proxy := http.NewServeMux()
	proxy.Handle("/vibe-remote/", http.StripPrefix("/vibe-remote", s.http.Handler))
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	endpoint := server.URL + "/vibe-remote/api/remote-sessions/console/console"
	read := func(cursor uint64) claude.TerminalChunk {
		t.Helper()
		request, _ := http.NewRequest("GET", endpoint, nil)
		request.Header.Set("X-Vibe-Remote", "1")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("read console = %d", response.StatusCode)
		}
		var chunk claude.TerminalChunk
		if err := json.NewDecoder(response.Body).Decode(&chunk); err != nil {
			t.Fatal(err)
		}
		return chunk
	}
	var first claude.TerminalChunk
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		first = read(0)
		if strings.Contains(string(first.Data), "Allow Notes") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(first.Data), "Allow Notes") || first.RunID == "" {
		t.Fatal("hidden approval prompt was not available through the dashboard")
	}
	request, _ := http.NewRequest("POST", endpoint, strings.NewReader(`{"runId":"`+first.RunID+`","key":"enter"}`))
	request.Header.Set("X-Vibe-Remote", "1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("send key = %d", response.StatusCode)
	}
	for time.Now().Before(deadline) {
		if strings.Contains(string(read(0).Data), "KEY_RECEIVED") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("confirmation key did not reach the worker PTY")
}
