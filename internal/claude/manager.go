package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

var (
	ErrInvalidPath = errors.New("unsafe path")
	ErrStopTimeout = errors.New("Claude worker did not stop before timeout")
)

// Store is the account persistence required by Manager. Implementations can
// adapt a broader application store without coupling this package to it.
type Store interface {
	UpsertAccount(context.Context, model.Account) (model.Account, error)
	GetAccount(context.Context, string) (model.Account, error)
	ListAccounts(context.Context) ([]model.Account, error)
	DeleteAccount(context.Context, string) error
	GetRemoteSession(context.Context, string) (model.RemoteSession, error)
}

type Options struct {
	ClaudePath      string
	Runner          CommandRunner
	StopTimeout     time.Duration
	RestartLimit    int
	InitialBackoff  time.Duration
	MaxBackoff      time.Duration
	StableRunWindow time.Duration
	ReadyTimeout    time.Duration
	Now             func() time.Time
}

type Manager struct {
	store        Store
	runner       CommandRunner
	profilesRoot string
	workerPIDDir string
	stopTimeout  time.Duration
	restartLimit int
	initialDelay time.Duration
	maxDelay     time.Duration
	stableWindow time.Duration
	readyTimeout time.Duration
	now          func() time.Time

	opMu    sync.Mutex
	mu      sync.RWMutex
	workers map[string]*worker
}

// WorkerSpec describes one Remote Control session slot to run. ID is the stable
// key that lets several slots coexist and be restarted independently.
type WorkerSpec struct {
	ID              string
	AccountID       string
	Workspace       string
	Name            string
	ResumeSessionID string
	Force           bool
}

type worker struct {
	id        string
	account   model.Account
	workspace string
	name      string
	resume    string
	ctx       context.Context
	cancel    context.CancelFunc
	process   Process
	done      chan struct{}
	startedAt time.Time
	state     model.WorkerStatus
}

func New(appDataRoot string, store Store, options Options) (*Manager, error) {
	if store == nil {
		return nil, errors.New("Claude manager requires a store")
	}
	root, err := safeRoot(appDataRoot)
	if err != nil {
		return nil, err
	}

	runner := options.Runner
	if runner == nil {
		binary := strings.TrimSpace(options.ClaudePath)
		if binary == "" {
			binary, err = exec.LookPath("claude")
			if err != nil {
				return nil, fmt.Errorf("find Claude Code executable: %w", err)
			}
		}
		runner = &execRunner{binary: binary}
	}

	manager := &Manager{
		store:        store,
		runner:       runner,
		profilesRoot: filepath.Join(root, "claude-profiles"),
		workerPIDDir: filepath.Join(root, "worker-pids"),
		stopTimeout:  valueOr(options.StopTimeout, 5*time.Second),
		restartLimit: options.RestartLimit,
		initialDelay: valueOr(options.InitialBackoff, 500*time.Millisecond),
		maxDelay:     valueOr(options.MaxBackoff, 10*time.Second),
		stableWindow: valueOr(options.StableRunWindow, 30*time.Second),
		// A cold isolated profile spends its first startup fetching config and
		// starting MCP servers before Claude registers Remote Control; measured
		// at ~29s on a first-run profile, so keep well clear of that.
		readyTimeout: valueOr(options.ReadyTimeout, 90*time.Second),
		now:          options.Now,
		workers:      make(map[string]*worker),
	}
	if manager.restartLimit == 0 {
		manager.restartLimit = 5
	}
	if manager.restartLimit < 0 {
		return nil, errors.New("restart limit cannot be negative")
	}
	if manager.maxDelay < manager.initialDelay {
		return nil, errors.New("maximum restart backoff is shorter than initial backoff")
	}
	if manager.now == nil {
		manager.now = time.Now
	}
	if err := os.MkdirAll(manager.profilesRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create Claude profile root: %w", err)
	}
	if err := os.MkdirAll(manager.workerPIDDir, 0o700); err != nil {
		return nil, fmt.Errorf("create Claude worker record directory: %w", err)
	}
	if info, err := os.Lstat(manager.profilesRoot); err != nil {
		return nil, fmt.Errorf("inspect Claude profile root: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: Claude profile root must be a real directory", ErrInvalidPath)
	}
	if err := os.Chmod(manager.profilesRoot, 0o700); err != nil {
		return nil, fmt.Errorf("secure Claude profile root: %w", err)
	}
	if err := manager.cleanupOrphan(); err != nil {
		return nil, err
	}
	return manager, nil
}

// Add creates an isolated official Claude Code profile and performs Anthropic's
// own interactive subscription login flow.
func (m *Manager) Add(ctx context.Context, email string) (model.Account, error) {
	normalizedEmail, err := normalizeEmail(email)
	if err != nil {
		return model.Account{}, err
	}
	accounts, err := m.store.ListAccounts(ctx)
	if err != nil {
		return model.Account{}, fmt.Errorf("list Claude accounts: %w", err)
	}
	for _, account := range accounts {
		if strings.EqualFold(account.Email, normalizedEmail) {
			return model.Account{}, errors.New("this Claude account is already configured")
		}
	}
	id, err := newID()
	if err != nil {
		return model.Account{}, fmt.Errorf("create account ID: %w", err)
	}
	profileDir, err := m.profileDir(id)
	if err != nil {
		return model.Account{}, err
	}
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		return model.Account{}, fmt.Errorf("create Claude profile: %w", err)
	}
	now := m.now().UTC()
	account := model.Account{
		ID: id, Email: normalizedEmail, ProfileDir: profileDir,
		Status: model.AccountPending, CreatedAt: now, UpdatedAt: now,
	}
	account, err = m.store.UpsertAccount(ctx, account)
	if err != nil {
		_ = os.RemoveAll(profileDir)
		return model.Account{}, fmt.Errorf("save Claude account: %w", err)
	}

	account, err = m.Login(ctx, account.ID)
	if err != nil {
		return account, err
	}
	return account, nil
}

func (m *Manager) Login(ctx context.Context, accountID string) (model.Account, error) {
	account, err := m.account(ctx, accountID)
	if err != nil {
		return model.Account{}, err
	}
	login := Command{
		Args: []string{"auth", "login", "--claudeai", "--email", account.Email},
		Env:  m.profileEnv(account),
	}
	if err := m.runner.RunInteractive(ctx, login); err != nil {
		return m.recordAccountError(ctx, account, "Claude login did not complete", err)
	}
	return m.refresh(ctx, account)
}

// Refresh verifies the isolated profile using Claude's supported JSON status.
func (m *Manager) Refresh(ctx context.Context, accountID string) (model.Account, error) {
	account, err := m.account(ctx, accountID)
	if err != nil {
		return model.Account{}, err
	}
	return m.refresh(ctx, account)
}

func (m *Manager) refresh(ctx context.Context, account model.Account) (model.Account, error) {
	output, err := m.runner.Output(ctx, Command{
		Args: []string{"auth", "status", "--json"}, Env: m.profileEnv(account),
	})
	if err != nil {
		return m.recordAccountError(ctx, account, "Claude authentication status failed", err)
	}
	status, err := parseAuthStatus(output)
	if err != nil {
		return m.recordAccountError(ctx, account, "Claude returned an invalid authentication status", err)
	}
	account.UpdatedAt = m.now().UTC()
	account.LastError = ""
	if !status.LoggedIn {
		account.Status = model.AccountSignedOut
		if _, err := m.store.UpsertAccount(ctx, account); err != nil {
			return account, fmt.Errorf("save Claude account status: %w", err)
		}
		return account, errors.New("Claude profile is signed out")
	}
	if status.Email != "" && !strings.EqualFold(status.Email, account.Email) {
		_ = m.runner.RunInteractive(ctx, Command{
			Args: []string{"auth", "logout"}, Env: m.profileEnv(account),
		})
		return m.recordAccountError(ctx, account, "Claude signed in with a different email", nil)
	}
	account.Status = model.AccountAuthenticated
	if _, err := m.store.UpsertAccount(ctx, account); err != nil {
		return account, fmt.Errorf("save Claude account status: %w", err)
	}
	return account, nil
}

// Remove first asks the unmodified Claude binary to revoke its credential, then
// removes only the validated profile subtree owned by this application.
func (m *Manager) Remove(ctx context.Context, accountID string, force bool) error {
	return m.remove(ctx, accountID, force, true)
}

func (m *Manager) RemoveProfile(ctx context.Context, accountID string, force bool) error {
	return m.remove(ctx, accountID, force, false)
}

func (m *Manager) remove(ctx context.Context, accountID string, force, deleteRecord bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	account, err := m.account(ctx, accountID)
	if err != nil {
		return err
	}
	if m.hasWorkerForAccount(account.ID) {
		if err := m.stopAccountLocked(ctx, account.ID, force); err != nil {
			return err
		}
	}
	if err := m.runner.RunInteractive(ctx, Command{
		Args: []string{"auth", "logout"}, Env: m.profileEnv(account),
	}); err != nil && !force {
		return fmt.Errorf("Claude logout failed; profile retained: %w", err)
	}
	profileDir, err := m.validateAccountProfile(account)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(profileDir); err != nil {
		return fmt.Errorf("remove Claude profile: %w", err)
	}
	if deleteRecord {
		if err := m.store.DeleteAccount(ctx, account.ID); err != nil {
			return fmt.Errorf("delete Claude account record: %w", err)
		}
	}
	return nil
}

// Activate starts or reconciles one Remote Control session slot. Slots are keyed
// by spec.ID, so several accounts and workspaces can run at the same time.
func (m *Manager) Activate(ctx context.Context, spec WorkerSpec) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if !validSlotID(spec.ID) {
		return fmt.Errorf("%w: invalid remote session ID", ErrInvalidPath)
	}
	account, err := m.account(ctx, spec.AccountID)
	if err != nil {
		return err
	}
	account, err = m.refresh(ctx, account)
	if err != nil {
		return err
	}
	workspace, err := safeWorkspace(spec.Workspace)
	if err != nil {
		return err
	}
	resume, err := safeResumeID(spec.ResumeSessionID)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = "Claude remote"
	}

	m.mu.RLock()
	current := m.workers[spec.ID]
	// The conversation is deliberately not compared: the checkpoint hook links
	// a live slot to whatever conversation it is talking in, so a running slot
	// almost always holds a newer conversation than the one it launched with.
	// Restarting on that drift would cut the very session it is tracking. A
	// caller that means to change conversations stops the slot first.
	unchanged := current != nil && current.account.ID == account.ID && current.workspace == workspace &&
		current.name == name && current.state.Running
	m.mu.RUnlock()
	if unchanged && !spec.Force {
		return nil
	}
	if current != nil {
		if err := m.stopLocked(ctx, spec.ID, spec.Force); err != nil {
			return err
		}
	}
	if err := enableWorkspaceTools(account.ProfileDir, workspace); err != nil {
		return fmt.Errorf("enable Claude workspace tools: %w", err)
	}

	workerContext, cancel := context.WithCancel(context.Background())
	w := &worker{
		id: spec.ID, account: account, workspace: workspace, name: name, resume: resume,
		ctx: workerContext, cancel: cancel, done: make(chan struct{}),
	}
	w.state = model.WorkerStatus{
		ID: spec.ID, Name: name, AccountID: account.ID, WorkspacePath: workspace, State: "starting",
	}
	m.mu.Lock()
	m.workers[spec.ID] = w
	m.mu.Unlock()

	process, err := m.start(w)
	if err != nil {
		cancel()
		m.mu.Lock()
		delete(m.workers, spec.ID)
		m.mu.Unlock()
		return fmt.Errorf("start Claude Remote Control: %w", err)
	}
	go m.supervise(w, process)
	if err := m.waitReady(w, process); err != nil {
		_ = m.stopLocked(context.Background(), spec.ID, true)
		return fmt.Errorf("Claude Remote Control did not become ready: %w", err)
	}
	m.markRunning(w, process)
	return nil
}

// Deactivate stops the worker for one slot. Stopping a slot that is not running
// is not an error.
func (m *Manager) Deactivate(ctx context.Context, slotID string, force bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.stopLocked(ctx, slotID, force)
}

// DeactivateAccount stops every worker belonging to one Claude account.
func (m *Manager) DeactivateAccount(ctx context.Context, accountID string, force bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.stopAccountLocked(ctx, accountID, force)
}

func (m *Manager) stopAccountLocked(ctx context.Context, accountID string, force bool) error {
	for _, slotID := range m.slotsForAccount(accountID) {
		if err := m.stopLocked(ctx, slotID, force); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) slotsForAccount(accountID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	slots := make([]string, 0, len(m.workers))
	for id, w := range m.workers {
		if w.account.ID == accountID {
			slots = append(slots, id)
		}
	}
	sort.Strings(slots)
	return slots
}

func (m *Manager) stopLocked(ctx context.Context, slotID string, force bool) error {
	m.mu.Lock()
	w := m.workers[slotID]
	if w == nil {
		m.mu.Unlock()
		return nil
	}
	w.cancel()
	process := w.process
	w.state.Running = process != nil
	w.state.State = "stopping"
	m.mu.Unlock()

	if process != nil {
		if err := process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) && !force {
			return fmt.Errorf("interrupt Claude worker: %w", err)
		}
	}
	if waitFor(ctx, w.done, m.stopTimeout) {
		m.clearStopped(w)
		return nil
	}
	if !force {
		return ErrStopTimeout
	}
	if process != nil {
		if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("terminate Claude worker: %w", err)
		}
	}
	if waitFor(ctx, w.done, m.stopTimeout) {
		m.clearStopped(w)
		return nil
	}
	if process != nil {
		if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill Claude worker: %w", err)
		}
	}
	if !waitFor(ctx, w.done, m.stopTimeout) {
		return ErrStopTimeout
	}
	m.clearStopped(w)
	return nil
}

// Status reports one slot. An unknown slot reads as stopped.
func (m *Manager) Status(slotID string) model.WorkerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w := m.workers[slotID]
	if w == nil {
		return model.WorkerStatus{ID: slotID, State: "stopped"}
	}
	return w.state
}

// Statuses reports every known slot, ordered by slot ID.
func (m *Manager) Statuses() []model.WorkerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	statuses := make([]model.WorkerStatus, 0, len(m.workers))
	for _, w := range m.workers {
		statuses = append(statuses, w.state)
	}
	sort.Slice(statuses, func(left, right int) bool { return statuses[left].ID < statuses[right].ID })
	return statuses
}

// RunningCount reports how many slots are currently serving Remote Control.
func (m *Manager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, w := range m.workers {
		if w.state.Running {
			count++
		}
	}
	return count
}

func (m *Manager) Close(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	slots := make([]string, 0, len(m.workers))
	for id := range m.workers {
		slots = append(slots, id)
	}
	m.mu.RUnlock()
	sort.Strings(slots)
	var firstErr error
	for _, slotID := range slots {
		if err := m.stopLocked(ctx, slotID, true); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *Manager) start(w *worker) (Process, error) {
	resume := m.storedResume(w)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[w.id] != w || w.ctx.Err() != nil {
		return nil, context.Canceled
	}
	w.resume = resume
	arguments := []string{"--dangerously-skip-permissions", "--chrome", "--verbose"}
	if resume != "" {
		arguments = append(arguments, "--resume", resume)
	}
	arguments = append(arguments, "--remote-control", w.name)
	process, err := m.runner.Start(Command{
		Args: arguments,
		Dir:  w.workspace,
		Env:  m.workerEnv(w),
	})
	if err != nil {
		return nil, err
	}
	w.process = process
	if err := m.writeWorkerPID(w.id, process.PID()); err != nil {
		_ = process.Kill()
		return nil, err
	}
	w.startedAt = m.now()
	w.state = model.WorkerStatus{
		ID: w.id, Name: w.name, AccountID: w.account.ID, WorkspacePath: w.workspace,
		PID: process.PID(), State: "connecting",
	}
	return process, nil
}

// storedResume reports the conversation this slot should open. The stored slot
// row wins over the value the worker last launched with, because the checkpoint
// hook keeps it current: a supervised restart after a crash then continues the
// live conversation instead of silently opening an empty one.
//
// Anything unreadable or malformed falls back to the launch value rather than
// failing the start, and a slot with no stored row (the manager can be driven
// without one) keeps behaving exactly as its caller asked.
func (m *Manager) storedResume(w *worker) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stored, err := m.store.GetRemoteSession(ctx, w.id)
	if err != nil {
		return w.resume
	}
	resume, err := safeResumeID(stored.ResumeSessionID)
	if err != nil {
		return w.resume
	}
	return resume
}

func (m *Manager) supervise(w *worker, process Process) {
	defer close(w.done)
	attempts := 0
	delay := m.initialDelay
	for {
		err := process.Wait()
		if w.ctx.Err() != nil {
			m.markStopped(w)
			return
		}
		m.mu.RLock()
		startedAt := w.startedAt
		m.mu.RUnlock()
		if m.now().Sub(startedAt) >= m.stableWindow {
			attempts = 0
			delay = m.initialDelay
		}
		lastError := "Claude Remote Control exited unexpectedly"
		if err != nil {
			lastError = "Claude Remote Control exited unexpectedly: " + err.Error()
		}
		for {
			attempts++
			if attempts > m.restartLimit {
				m.markFailed(w, "Claude Remote Control stopped after repeated restart failures")
				return
			}
			m.markRestarting(w, lastError)
			if !waitDelay(w.ctx, delay) {
				m.markStopped(w)
				return
			}
			next, startErr := m.start(w)
			if startErr == nil {
				if readyErr := m.waitReady(w, next); readyErr == nil {
					process = next
					m.markRunning(w, next)
					delay = minDuration(delay*2, m.maxDelay)
					break
				} else {
					lastError = "Claude Remote Control restart did not become ready: " + readyErr.Error()
				}
			} else {
				lastError = "Claude Remote Control restart failed: " + startErr.Error()
			}
			delay = minDuration(delay*2, m.maxDelay)
		}
	}
}

func (m *Manager) waitReady(w *worker, process Process) error {
	timer := time.NewTimer(m.readyTimeout)
	defer timer.Stop()
	select {
	case <-process.Ready():
		if err := process.ReadyError(); err != nil {
			return err
		}
		return nil
	case <-w.ctx.Done():
		return context.Canceled
	case <-timer.C:
		return errors.New("registration timed out")
	}
}

func (m *Manager) markRunning(w *worker, process Process) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[w.id] == w && w.process == process {
		w.state = model.WorkerStatus{
			ID: w.id, Name: w.name, AccountID: w.account.ID, WorkspacePath: w.workspace,
			Running: true, PID: process.PID(), State: "running",
			RemoteURL: remoteControlURLForProcess(w.account.ProfileDir, process.PID(), process.RemoteURL()),
		}
	}
}

func remoteControlURLForProcess(profileDir string, pid int, fallback string) string {
	data, err := os.ReadFile(filepath.Join(profileDir, ".claude.json"))
	if err != nil {
		return fallback
	}
	var state struct {
		Bridges map[string]struct {
			PID int `json:"pid"`
		} `json:"replBridgePlaceholders"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return fallback
	}
	for bridgeID, bridge := range state.Bridges {
		if bridge.PID == pid && validBridgeSessionID(bridgeID) {
			return "https://claude.ai/code/" + bridgeID
		}
	}
	return fallback
}

func validBridgeSessionID(value string) bool {
	if !strings.HasPrefix(value, "cse_") || len(value) == len("cse_") {
		return false
	}
	for _, character := range value[len("cse_"):] {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func (m *Manager) account(ctx context.Context, accountID string) (model.Account, error) {
	if !validID(accountID) {
		return model.Account{}, fmt.Errorf("%w: invalid Claude account ID", ErrInvalidPath)
	}
	account, err := m.store.GetAccount(ctx, accountID)
	if err != nil {
		return model.Account{}, fmt.Errorf("load Claude account: %w", err)
	}
	if _, err := m.validateAccountProfile(account); err != nil {
		return model.Account{}, err
	}
	return account, nil
}

func (m *Manager) profileDir(accountID string) (string, error) {
	if !validID(accountID) {
		return "", fmt.Errorf("%w: invalid Claude account ID", ErrInvalidPath)
	}
	target := filepath.Join(m.profilesRoot, accountID)
	relative, err := filepath.Rel(m.profilesRoot, target)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("%w: Claude profile escapes profile root", ErrInvalidPath)
	}
	return target, nil
}

func (m *Manager) validateAccountProfile(account model.Account) (string, error) {
	expected, err := m.profileDir(account.ID)
	if err != nil {
		return "", err
	}
	if filepath.Clean(account.ProfileDir) != expected {
		return "", fmt.Errorf("%w: stored Claude profile path does not match account ID", ErrInvalidPath)
	}
	return expected, nil
}

func (m *Manager) profileEnv(account model.Account) []string {
	environment := filteredEnvironment(os.Environ())
	return append(environment,
		"CLAUDE_CONFIG_DIR="+account.ProfileDir,
		"VIBE_REMOTE_ACCOUNT_ID="+account.ID,
	)
}

// workerEnv adds the slot identity to the profile environment. Hooks inherit it
// from the Claude process, which is the only signal that distinguishes two slots
// sharing one account and workspace: their conversations are otherwise
// indistinguishable, and picking the most recent would attach the wrong one.
func (m *Manager) workerEnv(w *worker) []string {
	return append(m.profileEnv(w.account), "VIBE_REMOTE_SLOT_ID="+w.id)
}

func (m *Manager) recordAccountError(ctx context.Context, account model.Account, message string, cause error) (model.Account, error) {
	account.Status = model.AccountError
	account.LastError = message
	account.UpdatedAt = m.now().UTC()
	if _, err := m.store.UpsertAccount(ctx, account); err != nil {
		return account, fmt.Errorf("save Claude account error: %w", err)
	}
	if cause != nil {
		return account, fmt.Errorf("%s: %w", message, cause)
	}
	return account, errors.New(message)
}

func (m *Manager) hasWorkerForAccount(accountID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, w := range m.workers {
		if w.account.ID == accountID {
			return true
		}
	}
	return false
}

func (m *Manager) clearStopped(w *worker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[w.id] == w {
		m.removeWorkerPID(w.id, w.process)
		delete(m.workers, w.id)
	}
}

func (m *Manager) markStopped(w *worker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[w.id] == w {
		m.removeWorkerPID(w.id, w.process)
		w.state.Running = false
		w.state.PID = 0
		w.state.State = "stopped"
	}
}

func (m *Manager) markRestarting(w *worker, lastError string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[w.id] == w {
		w.state.Running = false
		w.state.PID = 0
		w.state.State = "restarting"
		w.state.LastError = lastError
	}
}

func (m *Manager) markFailed(w *worker, lastError string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[w.id] == w {
		m.removeWorkerPID(w.id, w.process)
		w.state.Running = false
		w.state.PID = 0
		w.state.State = "failed"
		w.state.LastError = lastError
	}
}

func (m *Manager) workerPIDPath(slotID string) (string, error) {
	if !validSlotID(slotID) {
		return "", fmt.Errorf("%w: invalid remote session ID", ErrInvalidPath)
	}
	return filepath.Join(m.workerPIDDir, slotID+".pid"), nil
}

func (m *Manager) writeWorkerPID(slotID string, pid int) error {
	path, err := m.workerPIDPath(slotID)
	if err != nil {
		return err
	}
	temporary := path + ".new"
	if err := os.WriteFile(temporary, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		return fmt.Errorf("record Claude worker PID: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("activate Claude worker PID: %w", err)
	}
	return nil
}

func (m *Manager) removeWorkerPID(slotID string, process Process) {
	if process == nil {
		return
	}
	path, err := m.workerPIDPath(slotID)
	if err != nil {
		return
	}
	data, readErr := os.ReadFile(path)
	if readErr == nil && strings.TrimSpace(string(data)) == fmt.Sprintf("%d", process.PID()) {
		_ = os.Remove(path)
	}
}

// cleanupOrphan terminates Remote Control workers left behind by a previous run
// of this service, including the legacy single-worker record.
func (m *Manager) cleanupOrphan() error {
	legacy := filepath.Join(filepath.Dir(m.workerPIDDir), "worker.pid")
	if err := m.cleanupOrphanRecord(legacy); err != nil {
		return err
	}
	entries, err := os.ReadDir(m.workerPIDDir)
	if err != nil {
		return fmt.Errorf("read prior Claude worker records: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		if err := m.cleanupOrphanRecord(filepath.Join(m.workerPIDDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) cleanupOrphanRecord(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read prior Claude worker PID: %w", err)
	}
	pid := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 1 {
		return errors.New("invalid prior Claude worker PID record")
	}
	command, commandErr := exec.Command("/bin/ps", "-p", fmt.Sprintf("%d", pid), "-o", "command=").Output()
	if commandErr == nil {
		text := string(command)
		if strings.Contains(text, "claude") && strings.Contains(text, "remote-control") {
			process, findErr := os.FindProcess(pid)
			if findErr == nil {
				_ = process.Signal(syscall.SIGTERM)
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if signalErr := process.Signal(syscall.Signal(0)); signalErr != nil {
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				if signalErr := process.Signal(syscall.Signal(0)); signalErr == nil {
					_ = process.Kill()
				}
			}
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove prior Claude worker PID: %w", err)
	}
	return nil
}

type authStatus struct {
	LoggedIn bool
	Email    string
}

func parseAuthStatus(data []byte) (authStatus, error) {
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return authStatus{}, err
	}
	loggedIn, ok := value["loggedIn"].(bool)
	if !ok {
		return authStatus{}, errors.New("missing loggedIn field")
	}
	return authStatus{LoggedIn: loggedIn, Email: findEmail(value)}, nil
}

func findEmail(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalizedKey := strings.ToLower(strings.ReplaceAll(key, "_", ""))
			if normalizedKey == "email" || normalizedKey == "emailaddress" || normalizedKey == "accountemail" {
				if email, ok := child.(string); ok {
					return strings.TrimSpace(email)
				}
			}
		}
		for _, child := range typed {
			if email := findEmail(child); email != "" {
				return email
			}
		}
	case []any:
		for _, child := range typed {
			if email := findEmail(child); email != "" {
				return email
			}
		}
	}
	return ""
}

func filteredEnvironment(input []string) []string {
	blocked := map[string]struct{}{
		"ANTHROPIC_API_KEY": {}, "ANTHROPIC_AUTH_TOKEN": {}, "ANTHROPIC_BASE_URL": {},
		"CLAUDE_CODE_OAUTH_TOKEN": {}, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN": {}, "CLAUDE_CODE_OAUTH_SCOPES": {},
		"CLAUDE_CODE_USE_BEDROCK": {}, "CLAUDE_CODE_USE_VERTEX": {}, "CLAUDE_CODE_USE_FOUNDRY": {},
		"CLAUDE_CONFIG_DIR": {}, "VIBE_REMOTE_ACCOUNT_ID": {}, "VIBE_REMOTE_SLOT_ID": {},
	}
	result := make([]string, 0, len(input))
	for _, item := range input {
		key, _, found := strings.Cut(item, "=")
		if _, skip := blocked[key]; found && skip {
			continue
		}
		result = append(result, item)
	}
	return result
}

func safeRoot(input string) (string, error) {
	if strings.TrimSpace(input) == "" || !filepath.IsAbs(input) {
		return "", fmt.Errorf("%w: application data root must be absolute", ErrInvalidPath)
	}
	root := filepath.Clean(input)
	if root == string(filepath.Separator) {
		return "", fmt.Errorf("%w: filesystem root is not an application data directory", ErrInvalidPath)
	}
	if home, err := os.UserHomeDir(); err == nil && root == filepath.Clean(home) {
		return "", fmt.Errorf("%w: home directory is not an application data directory", ErrInvalidPath)
	}
	return root, nil
}

func safeWorkspace(input string) (string, error) {
	if strings.TrimSpace(input) == "" || !filepath.IsAbs(input) {
		return "", fmt.Errorf("%w: workspace must be absolute", ErrInvalidPath)
	}
	workspace := filepath.Clean(input)
	if workspace == string(filepath.Separator) {
		return "", fmt.Errorf("%w: filesystem root cannot be a workspace", ErrInvalidPath)
	}
	if home, err := os.UserHomeDir(); err == nil && workspace == filepath.Clean(home) {
		return "", fmt.Errorf("%w: home directory cannot be a Remote Control workspace", ErrInvalidPath)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return "", fmt.Errorf("inspect workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: workspace is not a directory", ErrInvalidPath)
	}
	return workspace, nil
}

func normalizeEmail(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	address, err := mail.ParseAddress(trimmed)
	if err != nil || !strings.EqualFold(address.Address, trimmed) {
		return "", errors.New("invalid Claude account email")
	}
	return strings.ToLower(address.Address), nil
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// validSlotID keeps remote session IDs usable as filenames inside the worker
// record directory.
func validSlotID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, character := range id {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// safeResumeID accepts only the opaque session identifiers Claude prints, so a
// stored value can never turn into another command-line flag.
func safeResumeID(input string) (string, error) {
	resume := strings.TrimSpace(input)
	if resume == "" {
		return "", nil
	}
	if !model.ValidResumeSessionID(resume) {
		return "", errors.New("invalid Claude session ID")
	}
	return resume, nil
}

func validID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	for index, character := range id {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func waitFor(ctx context.Context, done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func waitDelay(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func valueOr(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}
