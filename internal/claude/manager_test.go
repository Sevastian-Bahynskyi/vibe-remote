package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestAddUsesIsolatedOfficialLoginAndVerifiesEmail(t *testing.T) {
	store := newFakeStore()
	runner := &fakeRunner{outputs: [][]byte{[]byte(`{"loggedIn":true,"account":{"email":"person@example.com"}}`)}}
	manager := newTestManager(t, store, runner)

	account, err := manager.Add(context.Background(), "Person@Example.com")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if account.Status != model.AccountAuthenticated {
		t.Fatalf("status = %q, want authenticated", account.Status)
	}
	if account.Email != "person@example.com" {
		t.Fatalf("email = %q", account.Email)
	}
	if !validID(account.ID) {
		t.Fatalf("ID %q is not a UUID", account.ID)
	}
	if info, statErr := os.Stat(account.ProfileDir); statErr != nil || !info.IsDir() {
		t.Fatalf("profile was not created: %v", statErr)
	}

	login := runner.interactiveAt(t, 0)
	wantArgs := []string{"auth", "login", "--claudeai", "--email", "person@example.com"}
	if strings.Join(login.Args, " ") != strings.Join(wantArgs, " ") {
		t.Fatalf("login args = %#v", login.Args)
	}
	assertEnv(t, login.Env, "CLAUDE_CONFIG_DIR", account.ProfileDir)
	assertEnv(t, login.Env, "VIBE_REMOTE_ACCOUNT_ID", account.ID)
	status := runner.outputAt(t, 0)
	assertEnv(t, status.Env, "CLAUDE_CONFIG_DIR", account.ProfileDir)
}

func TestAddRejectsDifferentReturnedEmailAndLogsOut(t *testing.T) {
	store := newFakeStore()
	runner := &fakeRunner{outputs: [][]byte{[]byte(`{"loggedIn":true,"email":"other@example.com"}`)}}
	manager := newTestManager(t, store, runner)

	account, err := manager.Add(context.Background(), "wanted@example.com")
	if err == nil || !strings.Contains(err.Error(), "different email") {
		t.Fatalf("Add() error = %v", err)
	}
	if account.Status != model.AccountError {
		t.Fatalf("status = %q, want error", account.Status)
	}
	if runner.interactiveCount() < 2 {
		t.Fatal("expected official logout after mismatched identity")
	}
	logout := runner.interactiveAt(t, 1)
	if strings.Join(logout.Args, " ") != "auth logout" {
		t.Fatalf("second command = %#v, want auth logout", logout.Args)
	}
}

func TestRemoveRejectsStoredPathOutsideProfileRoot(t *testing.T) {
	store := newFakeStore()
	runner := &fakeRunner{}
	manager := newTestManager(t, store, runner)
	id := "12345678-1234-4123-8123-123456789abc"
	store.accounts[id] = model.Account{ID: id, Email: "person@example.com", ProfileDir: t.TempDir()}

	err := manager.Remove(context.Background(), id, true)
	if !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Remove() error = %v, want ErrInvalidPath", err)
	}
	if runner.interactiveCount() != 0 {
		t.Fatal("unsafe account path reached Claude runner")
	}
}

func TestRemoveLogsOutThenDeletesOnlyProfile(t *testing.T) {
	store := newFakeStore()
	runner := &fakeRunner{}
	manager := newTestManager(t, store, runner)
	id := "12345678-1234-4123-8123-123456789abc"
	profile, err := manager.profileDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "state"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	neighbor := filepath.Join(manager.profilesRoot, "keep")
	if err := os.WriteFile(neighbor, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.accounts[id] = model.Account{ID: id, Email: "person@example.com", ProfileDir: profile}

	if err := manager.Remove(context.Background(), id, false); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("profile still exists: %v", err)
	}
	if _, err := os.Stat(neighbor); err != nil {
		t.Fatalf("neighbor was removed: %v", err)
	}
	if got := strings.Join(runner.interactiveAt(t, 0).Args, " "); got != "auth logout" {
		t.Fatalf("command = %q", got)
	}
}

func TestActivateStartsRemoteControlAndExportsAccountID(t *testing.T) {
	store := newFakeStore()
	process := newFakeProcess(101, true, true)
	runner := &fakeRunner{
		outputs:   [][]byte{[]byte(`{"loggedIn":true,"email":"person@example.com"}`)},
		processes: []Process{process},
	}
	manager := newTestManager(t, store, runner)
	account := addStoredAccount(t, manager, store)
	workspace := t.TempDir()

	if err := manager.Activate(context.Background(), WorkerSpec{ID: "slot-a", AccountID: account.ID, Workspace: workspace, Name: "Project A"}); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	command := runner.startAt(t, 0)
	if got := strings.Join(command.Args, " "); got != "--dangerously-skip-permissions --chrome --verbose --remote-control Project A" {
		t.Fatalf("worker command = %q", got)
	}
	if command.Dir != workspace {
		t.Fatalf("worker dir = %q", command.Dir)
	}
	state, err := os.ReadFile(filepath.Join(account.ProfileDir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(state, &document); err != nil {
		t.Fatal(err)
	}
	project := document["projects"].(map[string]any)[workspace].(map[string]any)
	if project["hasTrustDialogAccepted"] != true {
		t.Fatalf("workspace was not trusted: %#v", project)
	}
	servers := project["enabledMcpServers"].([]any)
	if len(servers) != 1 || servers[0] != "computer-use" {
		t.Fatalf("enabled MCP servers = %#v", servers)
	}
	assertEnv(t, command.Env, "CLAUDE_CONFIG_DIR", account.ProfileDir)
	assertEnv(t, command.Env, "VIBE_REMOTE_ACCOUNT_ID", account.ID)
	if status := manager.Status("slot-a"); !status.Running || status.PID != 101 || status.AccountID != account.ID {
		t.Fatalf("status = %#v", status)
	} else if status.RemoteURL != "https://claude.ai/code/cse_test" {
		t.Fatalf("RemoteURL = %q", status.RemoteURL)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnableWorkspaceToolsIsIdempotentAndPreservesProjectState(t *testing.T) {
	t.Parallel()
	profile := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(profile, ".claude.json")
	document := map[string]any{
		"theme": "dark",
		"projects": map[string]any{
			workspace: map[string]any{
				"lastSessionId":     "session-1",
				"enabledMcpServers": []any{"existing-server"},
			},
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := enableWorkspaceTools(profile, workspace); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := enableWorkspaceTools(profile, workspace); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("second workspace tool installation changed Claude state")
	}

	var updated map[string]any
	if err := json.Unmarshal(second, &updated); err != nil {
		t.Fatal(err)
	}
	if updated["theme"] != "dark" {
		t.Fatal("unrelated Claude state was not preserved")
	}
	project := updated["projects"].(map[string]any)[workspace].(map[string]any)
	if project["lastSessionId"] != "session-1" || project["hasTrustDialogAccepted"] != true {
		t.Fatalf("project state was not preserved and trusted: %#v", project)
	}
	servers := project["enabledMcpServers"].([]any)
	if len(servers) != 2 || servers[0] != "existing-server" || servers[1] != "computer-use" {
		t.Fatalf("enabled MCP servers = %#v", servers)
	}
}

func TestEnableWorkspaceToolsRejectsMalformedState(t *testing.T) {
	t.Parallel()
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, ".claude.json"), []byte(`{"projects":"invalid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enableWorkspaceTools(profile, t.TempDir()); err == nil {
		t.Fatal("malformed Claude project state was silently replaced")
	}
}

func TestRemoteControlURLForProcessSelectsLiveBridge(t *testing.T) {
	t.Parallel()
	profile := t.TempDir()
	state := []byte(`{
  "replBridgePlaceholders": {
    "cse_stale": {"pid": 101},
    "cse_live123": {"pid": 202},
    "not-a-bridge": {"pid": 202}
  }
}`)
	if err := os.WriteFile(filepath.Join(profile, ".claude.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}

	got := remoteControlURLForProcess(profile, 202, "https://claude.ai/code")
	if got != "https://claude.ai/code/cse_live123" {
		t.Fatalf("remoteControlURLForProcess() = %q", got)
	}
}

func TestRemoteControlURLForProcessFallsBackWithoutMatchingBridge(t *testing.T) {
	t.Parallel()
	got := remoteControlURLForProcess(t.TempDir(), 202, "https://claude.ai/code")
	if got != "https://claude.ai/code" {
		t.Fatalf("remoteControlURLForProcess() = %q", got)
	}
}

func TestSupervisorRestartsAfterUnexpectedExit(t *testing.T) {
	store := newFakeStore()
	first := newFakeProcess(101, true, true)
	second := newFakeProcess(202, true, true)
	runner := &fakeRunner{
		outputs:   [][]byte{[]byte(`{"loggedIn":true,"email":"person@example.com"}`)},
		processes: []Process{first, second},
	}
	manager := newTestManager(t, store, runner)
	account := addStoredAccount(t, manager, store)

	if err := manager.Activate(context.Background(), WorkerSpec{ID: "slot-a", AccountID: account.ID, Workspace: t.TempDir(), Name: "Project"}); err != nil {
		t.Fatal(err)
	}
	first.exit(errors.New("boom"))
	waitUntil(t, time.Second, func() bool {
		status := manager.Status("slot-a")
		return status.Running && status.PID == 202
	})
	if runner.startCount() != 2 {
		t.Fatalf("start count = %d", runner.startCount())
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestActivateRunsSeveralSlotsAtOnce(t *testing.T) {
	store := newFakeStore()
	first := newFakeProcess(101, true, true)
	second := newFakeProcess(202, true, true)
	runner := &fakeRunner{
		outputs: [][]byte{
			[]byte(`{"loggedIn":true,"email":"first@example.com"}`),
			[]byte(`{"loggedIn":true,"email":"second@example.com"}`),
		},
		processes: []Process{first, second},
	}
	manager := newTestManager(t, store, runner)
	firstAccount := addStoredAccountWithID(t, manager, store, "12345678-1234-4123-8123-123456789abc", "first@example.com")
	secondAccount := addStoredAccountWithID(t, manager, store, "abcdefab-cdef-4abc-8def-abcdefabcdef", "second@example.com")
	workspace := t.TempDir()

	if err := manager.Activate(context.Background(), WorkerSpec{ID: "slot-a", AccountID: firstAccount.ID, Workspace: workspace, Name: "First"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(context.Background(), WorkerSpec{ID: "slot-b", AccountID: secondAccount.ID, Workspace: workspace, Name: "Second"}); err != nil {
		t.Fatal(err)
	}
	if signals := first.signalsSeen(); len(signals) != 0 {
		t.Fatalf("first worker was disturbed by a second slot: %#v", signals)
	}
	if manager.RunningCount() != 2 {
		t.Fatalf("running count = %d, want 2", manager.RunningCount())
	}
	if status := manager.Status("slot-a"); !status.Running || status.AccountID != firstAccount.ID || status.PID != 101 {
		t.Fatalf("slot-a status = %#v", status)
	}
	if status := manager.Status("slot-b"); !status.Running || status.AccountID != secondAccount.ID || status.PID != 202 {
		t.Fatalf("slot-b status = %#v", status)
	}
	if statuses := manager.Statuses(); len(statuses) != 2 {
		t.Fatalf("statuses = %#v", statuses)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestActivateReplacesTheWorkerHoldingTheSameSlot(t *testing.T) {
	store := newFakeStore()
	first := newFakeProcess(101, true, true)
	second := newFakeProcess(202, true, true)
	runner := &fakeRunner{
		outputs: [][]byte{
			[]byte(`{"loggedIn":true,"email":"first@example.com"}`),
			[]byte(`{"loggedIn":true,"email":"second@example.com"}`),
		},
		processes: []Process{first, second},
	}
	manager := newTestManager(t, store, runner)
	firstAccount := addStoredAccountWithID(t, manager, store, "12345678-1234-4123-8123-123456789abc", "first@example.com")
	secondAccount := addStoredAccountWithID(t, manager, store, "abcdefab-cdef-4abc-8def-abcdefabcdef", "second@example.com")
	workspace := t.TempDir()

	if err := manager.Activate(context.Background(), WorkerSpec{ID: "slot-a", AccountID: firstAccount.ID, Workspace: workspace, Name: "First"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(context.Background(), WorkerSpec{ID: "slot-a", AccountID: secondAccount.ID, Workspace: workspace, Name: "Second"}); err != nil {
		t.Fatal(err)
	}
	if signals := first.signalsSeen(); len(signals) == 0 || signals[0] != os.Interrupt {
		t.Fatalf("first worker signals = %#v", signals)
	}
	if status := manager.Status("slot-a"); !status.Running || status.AccountID != secondAccount.ID || status.PID != 202 {
		t.Fatalf("status = %#v", status)
	}
	if manager.RunningCount() != 1 {
		t.Fatalf("running count = %d, want 1", manager.RunningCount())
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestActivateResumesTheRequestedConversation(t *testing.T) {
	store := newFakeStore()
	runner := &fakeRunner{
		outputs:   [][]byte{[]byte(`{"loggedIn":true,"email":"person@example.com"}`)},
		processes: []Process{newFakeProcess(101, true, true)},
	}
	manager := newTestManager(t, store, runner)
	account := addStoredAccount(t, manager, store)

	err := manager.Activate(context.Background(), WorkerSpec{
		ID: "slot-a", AccountID: account.ID, Workspace: t.TempDir(),
		Name: "Project", ResumeSessionID: "9f2c1d3e-4b5a-4c6d-8e7f-0a1b2c3d4e5f",
	})
	if err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	want := "--dangerously-skip-permissions --chrome --verbose --resume 9f2c1d3e-4b5a-4c6d-8e7f-0a1b2c3d4e5f --remote-control Project"
	if got := strings.Join(runner.startAt(t, 0).Args, " "); got != want {
		t.Fatalf("worker command = %q, want %q", got, want)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestActivateRejectsUnsafeResumeIdentifier(t *testing.T) {
	store := newFakeStore()
	runner := &fakeRunner{outputs: [][]byte{[]byte(`{"loggedIn":true,"email":"person@example.com"}`)}}
	manager := newTestManager(t, store, runner)
	account := addStoredAccount(t, manager, store)

	err := manager.Activate(context.Background(), WorkerSpec{
		ID: "slot-a", AccountID: account.ID, Workspace: t.TempDir(),
		Name: "Project", ResumeSessionID: "--dangerously-skip-permissions",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid Claude session ID") {
		t.Fatalf("Activate() error = %v", err)
	}
	if runner.startCount() != 0 {
		t.Fatal("an unsafe resume identifier reached the Claude runner")
	}
}

func TestForceStopEscalatesFromInterruptToTerm(t *testing.T) {
	store := newFakeStore()
	process := newFakeProcess(101, false, true)
	runner := &fakeRunner{
		outputs:   [][]byte{[]byte(`{"loggedIn":true,"email":"person@example.com"}`)},
		processes: []Process{process},
	}
	manager := newTestManager(t, store, runner)
	account := addStoredAccount(t, manager, store)
	if err := manager.Activate(context.Background(), WorkerSpec{ID: "slot-a", AccountID: account.ID, Workspace: t.TempDir(), Name: "Project"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Deactivate(context.Background(), "slot-a", true); err != nil {
		t.Fatalf("Deactivate() error = %v", err)
	}
	signals := process.signalsSeen()
	if len(signals) < 2 || signals[0] != os.Interrupt || signals[1] != syscall.SIGTERM {
		t.Fatalf("signals = %#v, want Interrupt then SIGTERM", signals)
	}
}

func TestFilteredEnvironmentRemovesCredentialOverrides(t *testing.T) {
	input := []string{
		"PATH=/bin", "ANTHROPIC_API_KEY=secret", "ANTHROPIC_BASE_URL=https://proxy.invalid",
		"CLAUDE_CONFIG_DIR=/wrong", "VIBE_REMOTE_ACCOUNT_ID=wrong",
	}
	got := filteredEnvironment(input)
	if len(got) != 1 || got[0] != "PATH=/bin" {
		t.Fatalf("filtered environment = %#v", got)
	}
}

func newTestManager(t *testing.T, store *fakeStore, runner *fakeRunner) *Manager {
	t.Helper()
	manager, err := New(t.TempDir(), store, Options{
		Runner: runner, StopTimeout: 10 * time.Millisecond,
		InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
		StableRunWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func addStoredAccount(t *testing.T, manager *Manager, store *fakeStore) model.Account {
	t.Helper()
	return addStoredAccountWithID(t, manager, store, "12345678-1234-4123-8123-123456789abc", "person@example.com")
}

func addStoredAccountWithID(t *testing.T, manager *Manager, store *fakeStore, id, email string) model.Account {
	t.Helper()
	profile, err := manager.profileDir(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	account := model.Account{
		ID: id, Email: email, ProfileDir: profile,
		Status: model.AccountAuthenticated, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	store.accounts[id] = account
	return account
}

func assertEnv(t *testing.T, environment []string, key, want string) {
	t.Helper()
	prefix := key + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			if got := strings.TrimPrefix(item, prefix); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Fatalf("environment missing %s", key)
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met")
}

type fakeStore struct {
	mu       sync.Mutex
	accounts map[string]model.Account
}

func newFakeStore() *fakeStore {
	return &fakeStore{accounts: make(map[string]model.Account)}
}

func (s *fakeStore) UpsertAccount(_ context.Context, account model.Account) (model.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[account.ID] = account
	return account, nil
}

func (s *fakeStore) GetAccount(_ context.Context, id string) (model.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[id]
	if !ok {
		return model.Account{}, errors.New("not found")
	}
	return account, nil
}

func (s *fakeStore) ListAccounts(_ context.Context) ([]model.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := make([]model.Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func (s *fakeStore) DeleteAccount(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accounts, id)
	return nil
}

type fakeRunner struct {
	mu          sync.Mutex
	interactive []Command
	outputCalls []Command
	startCalls  []Command
	outputs     [][]byte
	processes   []Process
}

func (r *fakeRunner) RunInteractive(_ context.Context, command Command) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interactive = append(r.interactive, command)
	return nil
}

func (r *fakeRunner) Output(_ context.Context, command Command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outputCalls = append(r.outputCalls, command)
	if len(r.outputs) == 0 {
		return nil, errors.New("no fake output")
	}
	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return output, nil
}

func (r *fakeRunner) Start(command Command) (Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startCalls = append(r.startCalls, command)
	if len(r.processes) == 0 {
		return nil, errors.New("no fake process")
	}
	process := r.processes[0]
	r.processes = r.processes[1:]
	return process, nil
}

func (r *fakeRunner) interactiveAt(t *testing.T, index int) Command {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if index >= len(r.interactive) {
		t.Fatalf("interactive call %d missing", index)
	}
	return r.interactive[index]
}

func (r *fakeRunner) outputAt(t *testing.T, index int) Command {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if index >= len(r.outputCalls) {
		t.Fatalf("output call %d missing", index)
	}
	return r.outputCalls[index]
}

func (r *fakeRunner) startAt(t *testing.T, index int) Command {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if index >= len(r.startCalls) {
		t.Fatalf("start call %d missing", index)
	}
	return r.startCalls[index]
}

func (r *fakeRunner) interactiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.interactive)
}

func (r *fakeRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.startCalls)
}

type fakeProcess struct {
	mu                  sync.Mutex
	pid                 int
	exited              chan struct{}
	err                 error
	interruptTerminates bool
	termTerminates      bool
	signals             []os.Signal
	once                sync.Once
	ready               chan struct{}
	readyErr            error
	remoteURL           string
}

func newFakeProcess(pid int, interruptTerminates, termTerminates bool) *fakeProcess {
	process := &fakeProcess{
		pid: pid, exited: make(chan struct{}),
		interruptTerminates: interruptTerminates, termTerminates: termTerminates,
		ready: make(chan struct{}), remoteURL: "https://claude.ai/code/cse_test",
	}
	close(process.ready)
	return process
}

func (p *fakeProcess) PID() int { return p.pid }

func (p *fakeProcess) Ready() <-chan struct{} { return p.ready }

func (p *fakeProcess) ReadyError() error { return p.readyErr }

func (p *fakeProcess) RemoteURL() string { return p.remoteURL }

func (p *fakeProcess) Signal(signal os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, signal)
	p.mu.Unlock()
	if (signal == os.Interrupt && p.interruptTerminates) || (signal == syscall.SIGTERM && p.termTerminates) {
		p.exit(nil)
	}
	return nil
}

func (p *fakeProcess) Kill() error {
	p.exit(nil)
	return nil
}

func (p *fakeProcess) Wait() error {
	<-p.exited
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *fakeProcess) exit(err error) {
	p.once.Do(func() {
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.exited)
	})
}

func (p *fakeProcess) signalsSeen() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}
