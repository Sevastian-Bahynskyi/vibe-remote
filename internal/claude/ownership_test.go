package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func disposableWorker(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	data, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "claude-remote-control")
	if err := os.WriteFile(binary, data, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd, done
}

func TestAuthenticationAndManagerConstructionLeaveRecordedWorkersAlone(t *testing.T) {
	root := t.TempDir()
	cmd, _ := disposableWorker(t)
	if err := os.MkdirAll(filepath.Join(root, "worker-pids"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "worker-pids", "slot-a.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newFakeStore()
	runner := &fakeRunner{outputs: [][]byte{[]byte(`{"loggedIn":true,"email":"person@example.com"}`)}}
	accounts, err := NewAccounts(root, s, AuthOptions{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.Add(context.Background(), "person@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root, s, Options{Runner: runner}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("account helper terminated another worker")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("account helper removed another worker's record")
	}
}

func TestDaemonOwnershipExclusiveAndRequiredForRecovery(t *testing.T) {
	root := t.TempDir()
	first, err := AcquireOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := AcquireOwnership(root); err == nil {
		second.Close()
		t.Fatal("two daemons acquired ownership")
	}
	m, err := New(root, newFakeStore(), Options{Runner: &fakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RecoverWorkers(nil); err == nil {
		t.Fatal("recovery did not require ownership")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.RecoverWorkers(first); err == nil {
		t.Fatal("closed ownership authorized recovery")
	}
	second, err := AcquireOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := m.RecoverWorkers(second); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryVerifiesIdentityAndPreservesUnverifiableRecords(t *testing.T) {
	for _, kind := range []string{"verified", "legacy", "mismatch"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			m, err := New(root, newFakeStore(), Options{Runner: &fakeRunner{}})
			if err != nil {
				t.Fatal(err)
			}
			owner, err := AcquireOwnership(root)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			cmd, done := disposableWorker(t)
			identity, err := processIdentity(cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			record := workerRecord{PID: cmd.Process.Pid, Identity: identity, SlotID: "slot-a", AccountID: "account-a", RunID: "run-a"}
			if kind == "mismatch" {
				record.Identity = "reused-pid"
			}
			data, _ := json.Marshal(record)
			if kind == "legacy" {
				data = []byte(strconv.Itoa(cmd.Process.Pid))
			}
			path, _ := m.workerPIDPath("slot-a")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			err = m.RecoverWorkers(owner)
			if kind == "verified" {
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("verified orphan was not recovered")
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("recovered record was retained")
				}
			} else {
				if !errors.Is(err, ErrWorkerRepairRequired) {
					t.Fatalf("recovery error = %v", err)
				}
				if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
					t.Fatal("unverified process was terminated")
				}
				if err := m.checkRecoveryRecords("slot-a"); !errors.Is(err, ErrWorkerRepairRequired) {
					t.Fatal("unverified record did not block launch")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("unverified record was discarded")
				}
			}
		})
	}
}

func TestOldRunCannotRemoveReplacementWorkerRecord(t *testing.T) {
	m := newTestManager(t, newFakeStore(), &fakeRunner{})
	w := &worker{id: "slot-a", account: model.Account{ID: "account-a"}, activityRunID: "new-run"}
	if err := m.writeWorkerPID(w, 99999999); err != nil {
		t.Fatal(err)
	}
	old := *w
	old.activityRunID = "old-run"
	m.removeWorkerPID(&old, newFakeProcess(99999999, true, true))
	path, _ := m.workerPIDPath(w.id)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("old run removed replacement record")
	}
}
