package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

const appDialog = "Computer Use wants to control these apps\r\n  ● Notes\r\n❯ Deny, and tell Claude what to do differently (esc)\r\n  Allow for this session (1 app)\r\n"

func TestComputerUseApprovalRecognizesOnlyTheAppDialog(t *testing.T) {
	for _, test := range []struct{ screen, key string }{
		{appDialog, "\x1b[B"},
		{strings.ReplaceAll(strings.ReplaceAll(appDialog, "❯ Deny", "  Deny"), "  Allow", "❯ Allow"), "\r"},
		{strings.ReplaceAll(appDialog, "1 app", "0 apps"), ""},
		{strings.ReplaceAll(appDialog, "Computer Use wants to control these apps", "Computer Use needs macOS permissions"), ""},
		{appDialog + "… +2 more apps not shown, and not included in this grant.", ""},
		{"Do you want to run this command?\n❯ Allow for this session (1 app)", ""},
		{appDialog + appDialog, ""},
		{strings.ReplaceAll(appDialog, "these apps", "these\r\napps"), "\x1b[B"},
		{strings.ReplaceAll(strings.ReplaceAll(appDialog, "❯ Deny", "  Deny"), "  Allow for this session (1 app)", "❯ Allow for this session\r\n  (1 app)"), "\r"},
	} {
		if got := computerUseApprovalKey(test.screen); got != test.key {
			t.Errorf("approval key = %q, want %q", got, test.key)
		}
	}
}

func TestComputerUseApprovalSurvivesRepaintsWithoutSendingDuplicateKeys(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	request := "request-1"
	a := newComputerUseApprover(&execProcess{terminal: input}, func() string { return request })
	draw := func(dialog string) {
		t.Helper()
		_, _ = a.Write([]byte("\x1b[2J\x1b[H" + dialog))
		a.approve()
	}
	draw(appDialog)
	if a.navigated != "" {
		t.Fatal("approval did not wait for the dialog to settle")
	}
	a.stableAt = time.Time{}
	for i := 0; i < 5; i++ {
		draw(appDialog)
	}
	selected := strings.ReplaceAll(strings.ReplaceAll(appDialog, "❯ Deny", "  Deny"), "  Allow", "❯ Allow")
	draw(selected)
	a.stableAt = time.Time{}
	for i := 0; i < 5; i++ {
		draw(selected)
	}
	request = "request-2"
	draw(selected)
	a.stableAt = time.Time{}
	draw(selected)
	data, err := os.ReadFile(input.Name())
	if err != nil || string(data) != "\x1b[B\r\r" {
		t.Fatalf("approval keys = %q, error = %v", data, err)
	}
	request = ""
	draw(selected)
	if a.candidate != "" {
		t.Fatal("finished request retained its pending approval")
	}
}

func TestComputerUseApprovalWaitsForTheCurrentSelection(t *testing.T) {
	a := newComputerUseApprover(&execProcess{}, func() string { return "request" })
	_, _ = a.Write([]byte(appDialog))
	a.approve()
	a.stableAt = time.Time{}
	_, _ = a.Write([]byte("\x1b[2J\x1b[HUnrelated prompt"))
	a.approve()
	if a.candidate != "" || a.navigated != "" || a.answered != "" {
		t.Fatal("cleared dialog retained its pending approval")
	}
}

func TestComputerUseApprovalExpandsTruncatedAppDialog(t *testing.T) {
	terminal, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	defer slave.Close()
	process := &execProcess{terminal: terminal}
	a := newComputerUseApprover(process, func() string { return "request" })
	process.approval = a
	_, _ = a.Write([]byte(appDialog + "… +2 more apps not shown, and not\r\nincluded in this grant.\r\n"))
	a.approve()
	size, err := pty.GetsizeFull(terminal)
	if err != nil || size.Cols != 120 || size.Rows != 60 || a.navigated != "" {
		t.Fatalf("truncated dialog was not expanded before approving: %v, %v", size, err)
	}
	_, _ = a.Write([]byte("\x1b[2J\x1b[H" + appDialog))
	a.approve()
	a.stableAt = time.Time{}
	a.approve()
	if a.navigated != "request" {
		t.Fatal("expanded dialog was not approved")
	}
}

func TestComputerUseApprovalUsesCurrentScreenAndActiveTool(t *testing.T) {
	active := ""
	a := newComputerUseApprover(&execProcess{}, func() string { return active })
	for _, b := range []byte(appDialog) {
		_, _ = a.Write([]byte{b})
	}
	if !strings.Contains(a.screen.String(), "❯ Deny") {
		t.Fatal("split UTF-8 dialog was not reconstructed")
	}
	a.approve()
	if a.navigated != "" || a.answered != "" {
		t.Fatal("dialog approved without an active computer-use request")
	}
	_, _ = a.Write([]byte("\x1b[2J\x1b[HUnrelated prompt\r\n"))
	active = "request-1"
	a.approve()
	if a.navigated != "" || a.answered != "" {
		t.Fatal("old dialog in terminal history was approved")
	}
}

func TestComputerUseApprovalNavigatesThenApprovesThroughPTY(t *testing.T) {
	script := filepath.Join(t.TempDir(), "dialog")
	contents := `#!/bin/sh
stty raw -echo
printf 'Computer Use wants to control these apps\r\n  ● Notes\r\n❯ Deny, and tell Claude what to do differently (esc)\r\n  Allow for this session (1 app)\r\n'
key=$(dd bs=1 count=3 2>/dev/null)
test "$key" = "$(printf '\033[B')" || exit 1
printf '\033[2J\033[HComputer Use wants to control these apps\r\n  ● Notes\r\n  Deny, and tell Claude what to do differently (esc)\r\n❯ Allow for this session (1 app)\r\n'
key=$(dd bs=1 count=1 2>/dev/null)
test "$key" = "$(printf '\r')" || exit 2
printf '\033[2J\033[HAPP_APPROVED\r\n'
sleep 10
`
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &execRunner{override: script}
	process, err := runner.Start(Command{ComputerUseRequest: func() string { return "request-1" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Kill(); _ = process.Wait() })
	terminal := process.(terminalProcess)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(string(terminal.readTerminal(0).Data), "APP_APPROVED") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("automatic app approval did not reach the PTY: %s", terminal.readTerminal(0).Data)
}
