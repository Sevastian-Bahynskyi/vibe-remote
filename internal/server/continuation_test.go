package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/checkpoint"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

type continuationRunner struct {
	blockingRunner
	commands []claude.Command
	fail     bool
}

func (r *continuationRunner) Start(command claude.Command) (claude.Process, error) {
	r.commands = append(r.commands, command)
	if r.fail {
		return nil, errors.New("test startup failure")
	}
	p := &continuationProcess{blockingProcess: blockingProcess{ready: make(chan struct{}), done: make(chan struct{})}}
	close(p.ready)
	return p, nil
}

type continuationProcess struct{ blockingProcess }

func (p *continuationProcess) Signal(os.Signal) error { p.stop(); return nil }
func (p *continuationProcess) RemoteURL() string      { return "https://claude.ai/code/cse_test" }

func TestAccountContinuationCreatesNamedSlotAndRetriesSafely(t *testing.T) {
	const sourceAccount = "12345678-1234-4123-8123-123456789abc"
	const destinationAccount = "22345678-1234-4123-8123-123456789abc"
	for _, failFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "retry failed start"}[failFirst], func(t *testing.T) {
			runner := &continuationRunner{fail: failFirst}
			s, db := newTestServerWithStore(t, runner)
			ctx := context.Background()
			workspace := t.TempDir()
			for _, id := range []string{sourceAccount, destinationAccount} {
				_, err := db.UpsertAccount(ctx, model.Account{ID: id, Email: id + "@example.com", Status: model.AccountAuthenticated,
					ProfileDir: filepath.Join(s.layout.Profiles, id)})
				if err != nil {
					t.Fatal(err)
				}
			}
			source, _, err := db.RecordHookEvent(ctx, store.HookEvent{Provider: model.ProviderClaude, AccountID: sourceAccount,
				NativeSessionID: "2d758a1d-3630-4765-9f84-e91a8de810c7", WorkspacePath: workspace,
				Kind: store.HookEventPrompt, Prompt: "Finish the unfinished feature"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.UpsertRemoteSession(ctx, model.RemoteSession{ID: "original", Name: "Thinga", AccountID: sourceAccount,
				WorkspacePath: workspace, ResumeSessionID: source.NativeSessionID, Desired: model.DesiredStopped})
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.UpsertRemoteSession(ctx, model.RemoteSession{ID: "unrelated", Name: "Other work", AccountID: destinationAccount,
				WorkspacePath: workspace, Desired: model.DesiredStopped})
			if err != nil {
				t.Fatal(err)
			}
			handoff, _, err := s.createHandoffOp(ctx, source.ID, model.ProviderClaude, destinationAccount)
			if failFirst {
				if err == nil {
					t.Fatal("startup failure hidden")
				}
				runner.fail = false
				handoff, _, err = s.createHandoffOp(ctx, source.ID, model.ProviderClaude, destinationAccount)
			}
			if err != nil {
				t.Fatal(err)
			}
			remote, err := db.GetRemoteSession(ctx, handoff.ID)
			if err != nil || remote.Name != "Thinga" || remote.AccountID != destinationAccount || remote.WorkspacePath != source.WorkspacePath {
				t.Fatal("wrong destination", remote, err)
			}
			text, err := checkpoint.LoadContinuation(s.layout.Root, remote.ID)
			if err != nil || !strings.Contains(text, "Finish the unfinished feature") {
				t.Fatal("checkpoint missing", err)
			}
			command := runner.commands[len(runner.commands)-1]
			if command.Args[len(command.Args)-1] != checkpoint.ContinuationPrompt {
				t.Fatal("automatic prompt missing")
			}
			if strings.Contains(strings.Join(command.Args, " "), "unfinished feature") {
				t.Fatal("checkpoint exposed in process arguments")
			}
			before := len(runner.commands)
			if _, _, err := s.createHandoffOp(ctx, source.ID, model.ProviderClaude, destinationAccount); err != nil {
				t.Fatal(err)
			}
			if len(runner.commands) != before {
				t.Fatal("retry duplicated work")
			}
			other, _ := db.GetRemoteSession(ctx, "unrelated")
			if other.Desired != model.DesiredStopped {
				t.Fatal("unrelated destination modified")
			}
			pending, err := db.ListHandoffs(ctx, false)
			if err != nil || len(pending) != 0 {
				t.Fatal("automatic checkpoint exposed to manual continue")
			}
			remote.AccountID = sourceAccount
			if _, err := db.UpsertRemoteSession(ctx, remote); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.createHandoffOp(ctx, source.ID, model.ProviderClaude, destinationAccount); err == nil {
				t.Fatal("reused a destination that was moved to another account")
			}
		})
	}
}
