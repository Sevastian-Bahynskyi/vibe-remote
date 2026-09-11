package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

// TestServePreview runs the dashboard on a spare port for manual inspection.
// It never runs unless PREVIEW_ADDR is set.
func TestServePreview(t *testing.T) {
	addr := os.Getenv("PREVIEW_ADDR")
	if addr == "" {
		t.Skip("set PREVIEW_ADDR to preview the dashboard")
	}
	service, database := newTestServerWithStore(t, inertRunner{})
	remote := seedSession(t, database)
	ctx := t.Context()
	if _, err := database.UpsertSession(ctx, model.Session{
		Provider: model.ProviderClaude, NativeSessionID: "conv-abc", AccountID: remote.AccountID,
		Title: "Rework the onboarding flow", WorkspacePath: remote.WorkspacePath,
		Branch: "feature/onboarding", State: model.SessionCompleted, UpdatedAt: time.Now().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertRemoteSession(ctx, model.RemoteSession{
		Name: "Dev menu", AccountID: remote.AccountID, WorkspaceID: remote.WorkspaceID,
		WorkspacePath: remote.WorkspacePath, Desired: model.DesiredRunning,
	}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	// A second port with frame-ancestors relaxed, so the layout can be measured
	// inside a narrow iframe. Only the real handler above is what ships; this
	// exists because Chrome will not shrink a window to phone width.
	if framed := os.Getenv("PREVIEW_FRAME_ADDR"); framed != "" {
		frameListener, frameErr := net.Listen("tcp4", framed)
		if frameErr != nil {
			t.Fatal(frameErr)
		}
		go func() {
			_ = http.Serve(frameListener, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				recorder := httptest.NewRecorder()
				service.http.Handler.ServeHTTP(recorder, request)
				for name, values := range recorder.Header() {
					if name == "Content-Security-Policy" || name == "X-Frame-Options" {
						continue
					}
					for _, value := range values {
						response.Header().Add(name, value)
					}
				}
				response.WriteHeader(recorder.Code)
				_, _ = response.Write(recorder.Body.Bytes())
			}))
		}()
	}
	t.Log("preview on http://" + addr + "/")
	_ = http.Serve(listener, service.http.Handler)
}
