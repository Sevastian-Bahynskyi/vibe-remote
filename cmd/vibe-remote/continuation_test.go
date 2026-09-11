package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/checkpoint"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/paths"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func TestHookDeliversContinuationAndPersistsResume(t *testing.T) {
	root := t.TempDir()
	layout := paths.Layout{Root: root, Database: filepath.Join(root, "state.sqlite3")}
	db, err := store.Open(layout.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	account, err := db.UpsertAccount(ctx, model.Account{ID: "destination", Email: "destination@example.com", ProfileDir: root, Status: model.AccountAuthenticated})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := db.UpsertRemoteSession(ctx, model.RemoteSession{ID: "slot-a", Name: "Thinga", AccountID: account.ID, WorkspacePath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SaveContinuation(root, remote.ID, "The next step is to verify the unfinished feature."); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIBE_REMOTE_ACCOUNT_ID", account.ID)
	t.Setenv("VIBE_REMOTE_SLOT_ID", remote.ID)
	const nativeID = "2d758a1d-3630-4765-9f84-e91a8de810c7"
	payload, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "session_id": nativeID, "cwd": root, "prompt": checkpoint.ContinuationPrompt})
	invoke := func() string {
		t.Helper()
		input, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
		_, _ = writer.Write(payload)
		writer.Close()
		output, readerWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer output.Close()
		previousInput, previousOutput := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = input, readerWriter
		err = runHook(layout, []string{"claude", "UserPromptSubmit"})
		os.Stdin, os.Stdout = previousInput, previousOutput
		readerWriter.Close()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(output)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if text := invoke(); !strings.Contains(text, "verify the unfinished feature") {
		t.Fatal("checkpoint not delivered")
	}
	if text, err := checkpoint.LoadContinuation(root, remote.ID); err != nil || text != "" {
		t.Fatal("checkpoint not consumed", err)
	}
	linked, err := db.GetRemoteSession(ctx, remote.ID)
	if err != nil || linked.ResumeSessionID != nativeID {
		t.Fatal("destination does not resume its conversation", err)
	}
	if text := invoke(); text != "" {
		t.Fatal("checkpoint replayed after delivery")
	}
}
