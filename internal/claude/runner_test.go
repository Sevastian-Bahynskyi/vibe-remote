package claude

import (
	"strings"
	"testing"
)

func TestReadinessWriterRecognizesCurrentRemoteControlSuccess(t *testing.T) {
	t.Parallel()
	process := &execProcess{ready: make(chan struct{})}
	writer := &readinessWriter{process: process}
	parts := []string{
		"Take this session with you and pick up right where you left off on any device.\n",
		"The session keeps running on this machine. Press Ctrl+C to stop.\n",
	}
	for _, part := range parts {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-process.Ready():
		if err := process.ReadyError(); err != nil {
			t.Fatalf("unexpected readiness error: %v", err)
		}
	default:
		t.Fatal("current Claude Remote Control success text was not recognized")
	}
}

func TestReadinessWriterRequiresCompleteSuccessText(t *testing.T) {
	t.Parallel()
	process := &execProcess{ready: make(chan struct{})}
	writer := &readinessWriter{process: process}
	_, _ = writer.Write([]byte("Take this session with you.\n"))
	select {
	case <-process.Ready():
		t.Fatal("partial success text was accepted")
	default:
	}
}

func TestReadinessWriterSanitizesKnownFailure(t *testing.T) {
	t.Parallel()
	process := &execProcess{ready: make(chan struct{})}
	writer := &readinessWriter{process: process}
	_, _ = writer.Write([]byte("Please trust this folder before continuing"))
	<-process.Ready()
	if err := process.ReadyError(); err == nil || !strings.Contains(err.Error(), "workspace approval") {
		t.Fatalf("unexpected readiness error: %v", err)
	}
}
