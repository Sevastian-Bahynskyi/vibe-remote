package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrWorkerRepairRequired = errors.New("prior worker identity could not be verified; repair is required before starting this session")

type Ownership struct {
	mu   sync.Mutex
	file *os.File
	root string
}

func AcquireOwnership(appDataRoot string) (*Ownership, error) {
	root, err := safeRoot(appDataRoot)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errors.New("daemon ownership directory is unavailable")
	}
	fd, err := syscall.Open(filepath.Join(root, "daemon.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("could not open daemon ownership lock")
	}
	file := os.NewFile(uintptr(fd), "daemon.lock")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		_ = file.Close()
		return nil, errors.New("daemon ownership lock is unsafe")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, errors.New("could not secure daemon ownership lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("another Vibe Remote daemon owns this installation")
	}
	return &Ownership{file: file, root: root}, nil
}

func (o *Ownership) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file == nil {
		return nil
	}
	err := o.file.Close()
	o.file = nil
	return err
}

func (m *Manager) RecoverWorkers(owner *Ownership) error {
	if owner == nil {
		return errors.New("worker recovery requires daemon ownership")
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	root, err := filepath.EvalSymlinks(filepath.Dir(m.workerPIDDir))
	if err != nil || owner.file == nil || root != owner.root {
		return errors.New("worker recovery requires matching daemon ownership")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	paths := []string{filepath.Join(root, "worker.pid")}
	entries, err := os.ReadDir(m.workerPIDDir)
	if err != nil {
		return errors.New("could not read prior worker records")
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".pid") {
			paths = append(paths, filepath.Join(m.workerPIDDir, entry.Name()))
		}
	}
	var failures []error
	for _, path := range paths {
		if err := recoverWorkerRecord(path); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

type workerRecord struct {
	PID       int    `json:"pid"`
	Identity  string `json:"identity"`
	SlotID    string `json:"slotId"`
	AccountID string `json:"accountId"`
	RunID     string `json:"runId"`
}

func processIdentity(pid int) (string, error) {
	if pid <= 1 {
		return "", ErrWorkerRepairRequired
	}
	output, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "uid=,lstart=,comm=").Output()
	if err != nil {
		return "", ErrWorkerRepairRequired
	}
	fields := strings.Fields(string(output))
	if len(fields) < 7 || fields[0] != strconv.Itoa(os.Getuid()) {
		return "", ErrWorkerRepairRequired
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(string(output))))
	return hex.EncodeToString(digest[:]), nil
}

func (m *Manager) writeWorkerPID(w *worker, pid int) error {
	path, err := m.workerPIDPath(w.id)
	if err != nil {
		return err
	}
	identity, _ := processIdentity(pid)
	record := workerRecord{PID: pid, Identity: identity, SlotID: w.id, AccountID: w.account.ID, RunID: w.activityRunID}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(m.workerPIDDir, ".worker-pid-*")
	if err != nil {
		return errors.New("could not record Claude worker identity")
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return errors.New("could not record Claude worker identity")
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (m *Manager) removeWorkerPID(w *worker, process Process) {
	if process == nil {
		return
	}
	path, err := m.workerPIDPath(w.id)
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	var record workerRecord
	if err == nil && json.Unmarshal(data, &record) == nil && record.PID == process.PID() && record.RunID == w.activityRunID && record.SlotID == w.id && record.AccountID == w.account.ID {
		_ = os.Remove(path)
	}
}

func (m *Manager) checkRecoveryRecords(slotID string) error {
	path, err := m.workerPIDPath(slotID)
	if err != nil {
		return err
	}
	m.mu.RLock()
	hasWorker := m.workers[slotID] != nil
	m.mu.RUnlock()
	if hasWorker {
		return nil
	}
	for _, record := range []string{path, filepath.Join(filepath.Dir(m.workerPIDDir), "worker.pid")} {
		if _, err := os.Lstat(record); !errors.Is(err, os.ErrNotExist) {
			return ErrWorkerRepairRequired
		}
	}
	return nil
}

func recoverWorkerRecord(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return ErrWorkerRepairRequired
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ErrWorkerRepairRequired
	}
	var record workerRecord
	if json.Unmarshal(data, &record) != nil {
		record.PID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	if record.PID <= 1 {
		return ErrWorkerRepairRequired
	}
	process, err := os.FindProcess(record.PID)
	if err != nil {
		return ErrWorkerRepairRequired
	}
	if err := process.Signal(syscall.Signal(0)); errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return os.Remove(path)
	} else if err != nil {
		return ErrWorkerRepairRequired
	}
	identity, err := processIdentity(record.PID)
	if err != nil || record.Identity == "" || record.Identity != identity || record.SlotID == "" || record.AccountID == "" || record.RunID == "" {
		return ErrWorkerRepairRequired
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return ErrWorkerRepairRequired
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := process.Signal(syscall.Signal(0)); errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return os.Remove(path)
		}
		time.Sleep(50 * time.Millisecond)
	}
	identity, err = processIdentity(record.PID)
	if err != nil || identity != record.Identity {
		return ErrWorkerRepairRequired
	}
	if err := process.Kill(); err != nil && !errors.Is(err, syscall.ESRCH) {
		return ErrWorkerRepairRequired
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := process.Signal(syscall.Signal(0)); errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return os.Remove(path)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return ErrWorkerRepairRequired
}
