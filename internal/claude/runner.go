package claude

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/creack/pty"
)

// Command describes one invocation of the unmodified Claude Code binary.
type Command struct {
	Args []string
	Dir  string
	Env  []string
}

// Process is the subset of os.Process behavior needed by the worker supervisor.
type Process interface {
	PID() int
	Ready() <-chan struct{}
	ReadyError() error
	RemoteURL() string
	Signal(os.Signal) error
	Kill() error
	Wait() error
}

// CommandRunner keeps process execution injectable without exposing credentials
// or relying on Claude's private on-disk formats.
type CommandRunner interface {
	RunInteractive(context.Context, Command) error
	Output(context.Context, Command) ([]byte, error)
	Start(Command) (Process, error)
}

type execRunner struct {
	binary string
}

func (r *execRunner) RunInteractive(ctx context.Context, command Command) error {
	cmd := exec.CommandContext(ctx, r.binary, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	// Login opens Anthropic's browser flow. Suppress terminal output so OAuth
	// URLs or one-time codes never land in launchd's service logs.
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func (r *execRunner) Output(ctx context.Context, command Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.binary, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	return cmd.Output()
}

func (r *execRunner) Start(command Command) (Process, error) {
	cmd := exec.Command(r.binary, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	process := &execProcess{cmd: cmd, ready: make(chan struct{}), done: make(chan struct{})}
	observer := &readinessWriter{process: process}
	// Inspect output for the short-lived Remote Control URL. It is kept only in
	// process memory so the tailnet dashboard can open the registered session.
	terminal, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	process.terminal = terminal
	go func() {
		_, _ = io.Copy(observer, terminal)
	}()
	go process.reap()
	return process, nil
}

type execProcess struct {
	cmd        *exec.Cmd
	terminal   *os.File
	ready      chan struct{}
	done       chan struct{}
	readyOnce  sync.Once
	mu         sync.Mutex
	readyErr   error
	waitErr    error
	diagnostic string
	remoteURL  string
}

func (p *execProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *execProcess) Ready() <-chan struct{} { return p.ready }

func (p *execProcess) ReadyError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readyErr
}

func (p *execProcess) RemoteURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.remoteURL
}

func (p *execProcess) Signal(signal os.Signal) error {
	return p.cmd.Process.Signal(signal)
}

func (p *execProcess) Kill() error {
	return p.cmd.Process.Kill()
}

func (p *execProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *execProcess) markReady(err error) {
	p.readyOnce.Do(func() {
		p.mu.Lock()
		p.readyErr = err
		p.mu.Unlock()
		close(p.ready)
	})
}

func (p *execProcess) reap() {
	err := p.cmd.Wait()
	if p.terminal != nil {
		_ = p.terminal.Close()
	}
	p.mu.Lock()
	if p.diagnostic != "" {
		err = errors.New(p.diagnostic)
	}
	p.waitErr = err
	p.mu.Unlock()
	if err == nil {
		err = context.Canceled
	}
	p.markReady(err)
	close(p.done)
}

func (p *execProcess) setDiagnostic(message string) {
	p.mu.Lock()
	p.diagnostic = message
	p.mu.Unlock()
	p.markReady(errors.New(message))
}

func (p *execProcess) setRemoteURL(remoteURL string) {
	p.mu.Lock()
	p.remoteURL = remoteURL
	p.mu.Unlock()
	p.markReady(nil)
}

type readinessWriter struct {
	mu      sync.Mutex
	process *execProcess
	buffer  string
}

var remoteControlURLPattern = regexp.MustCompile(`https://claude(?:\.ai|\.com)/code/[^\s\x1b]+`)

func extractRemoteControlURL(output string) string {
	for _, match := range remoteControlURLPattern.FindAllString(output, -1) {
		candidate := strings.TrimRight(match, `.,;:)]}>"'`)
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Scheme != "https" {
			continue
		}
		if parsed.Hostname() != "claude.ai" && parsed.Hostname() != "claude.com" {
			continue
		}
		if parsed.Path == "/code/" || !strings.HasPrefix(parsed.Path, "/code/") {
			continue
		}
		return parsed.String()
	}
	plain := strings.ToLower(output)
	registered := strings.Contains(plain, "take this session with you") &&
		strings.Contains(plain, "claude.ai/code") &&
		strings.Contains(plain, "press ctrl+c to stop")
	if registered {
		return "https://claude.ai/code"
	}
	return ""
}

func (w *readinessWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer += string(data)
	if len(w.buffer) > 8192 {
		w.buffer = w.buffer[len(w.buffer)-8192:]
	}
	plain := strings.ToLower(w.buffer)
	for fragment, message := range map[string]string{
		"trust this folder":                 "workspace approval is required locally",
		"workspace trust":                   "workspace approval is required locally",
		"must be logged in":                 "Claude account is signed out",
		"requires a claude.ai subscription": "Claude account is not eligible for Remote Control",
		"disabled by your organization":     "Remote Control is disabled by the Claude organization",
		"isn't enabled for this account":    "Remote Control is not enabled for this Claude account",
		"couldn't verify remote control":    "Claude could not verify Remote Control eligibility",
		"remote credentials fetch failed":   "Claude could not establish Remote Control; check network and account eligibility",
	} {
		if strings.Contains(plain, fragment) {
			w.process.setDiagnostic(message)
			return len(data), nil
		}
	}
	if remoteURL := extractRemoteControlURL(w.buffer); remoteURL != "" {
		w.process.setRemoteURL(remoteURL)
	}
	return len(data), nil
}
