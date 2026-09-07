package claude

import (
	"strings"
	"testing"
	"time"
)

func TestExecRunnerStartsClaudeWithTerminal(t *testing.T) {
	t.Parallel()
	runner := &execRunner{binary: "/bin/sh"}
	process, err := runner.Start(Command{Args: []string{"-c", `
if [ -t 0 ] && [ -t 1 ]; then
  printf 'Take this session with you. Open claude.ai/code. Press Ctrl+C to stop.\n'
  sleep 30
else
  printf 'interactive terminal missing\n'
  exit 12
fi
`}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Kill()
		_ = process.Wait()
	})

	select {
	case <-process.Ready():
		if err := process.ReadyError(); err != nil {
			t.Fatalf("interactive process did not become ready: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interactive process did not emit readiness output")
	}
}

func TestReadinessWriterCapturesCurrentRemoteControlURL(t *testing.T) {
	t.Parallel()
	process := &execProcess{ready: make(chan struct{})}
	writer := &readinessWriter{process: process}
	parts := []string{
		"Take this session with you and pick up right where you left off on any device.\n",
		"Open the Code tab in the Claude mobile app, or visit claude.ai/code in a browser.\n",
		"The session keeps running on this machine. Use your other devices as a remote control. Press Ctrl+C to stop.\n",
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
		t.Fatal("current Claude Remote Control URL was not recognized")
	}
	if got := process.RemoteURL(); got != "https://claude.ai/code" {
		t.Fatalf("RemoteURL() = %q", got)
	}
}

func TestReadinessWriterPrefersDirectRemoteControlURL(t *testing.T) {
	t.Parallel()
	process := &execProcess{ready: make(chan struct{})}
	writer := &readinessWriter{process: process}
	_, _ = writer.Write([]byte("Open https://claude.ai/code/cse_test-session on another device.\n"))
	<-process.Ready()
	if got := process.RemoteURL(); got != "https://claude.ai/code/cse_test-session" {
		t.Fatalf("RemoteURL() = %q", got)
	}
}

func TestReadinessWriterRequiresConnectableURL(t *testing.T) {
	t.Parallel()
	process := &execProcess{ready: make(chan struct{})}
	writer := &readinessWriter{process: process}
	_, _ = writer.Write([]byte("Take this session with you. Press Ctrl+C to stop.\n"))
	select {
	case <-process.Ready():
		t.Fatal("success text without a connectable URL was accepted")
	default:
	}
}

func TestReadinessWriterRejectsUntrustedRemoteControlURL(t *testing.T) {
	t.Parallel()
	process := &execProcess{ready: make(chan struct{})}
	writer := &readinessWriter{process: process}
	_, _ = writer.Write([]byte("Open https://example.com/code/cse_not-claude\n"))
	select {
	case <-process.Ready():
		t.Fatal("untrusted Remote Control URL was accepted")
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
