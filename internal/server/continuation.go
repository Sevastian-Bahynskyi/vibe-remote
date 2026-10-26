package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/checkpoint"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

// The caller holds opMu. Retries reuse the destination conversation, including
// when a late source Stop hook refreshes the source checkpoint timestamp.
func (s *Server) continueClaude(ctx context.Context, source model.Session, account model.Account) (model.Handoff, string, error) {
	failure := func(message string) (model.Handoff, string, error) {
		return model.Handoff{}, "", failText(http.StatusBadRequest, message)
	}
	if source.AccountID == account.ID && source.Provider == model.ProviderClaude {
		return failure("Choose another Claude account to continue this conversation.")
	}
	key := sha256.Sum256([]byte(source.ID + "\x00" + account.ID))
	slotID := fmt.Sprintf("rs_%x", key[:16])
	handoff := model.Handoff{ID: slotID, SourceSessionID: source.ID, DestinationProvider: model.ProviderClaude,
		DestinationAccountID: account.ID, WorkspacePath: source.WorkspacePath}
	remote, err := s.store.GetRemoteSession(ctx, slotID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return failure("Could not read the continuation session.")
	}
	if err == nil {
		if remote.AccountID != account.ID || remote.WorkspacePath != source.WorkspacePath {
			return failure("The destination session was moved. Remove that slot before creating this continuation again.")
		}
		if s.claude.Status(slotID).Running {
			return handoff, "Claude", nil
		}
		// A retry keeps the destination's resolved intent, even if the source changes.
		if remote.RequestedModel == "" || remote.RequestedEffort == "" {
			selection, selectErr := s.store.ResolveRemoteSelection(ctx, remote, model.SelectionOverride{})
			if selectErr != nil {
				return failure("Could not read destination agent settings.")
			}
			if err := s.store.SetRemoteRequestedSelection(ctx, remote.ID, selection); err != nil {
				return failure("Could not update destination settings.")
			}
			remote.Model, remote.Effort = selection.Model, selection.Effort
			remote.RequestedModel, remote.RequestedEffort = selection.Model, selection.Effort
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		name := source.Title
		remotes, err := s.store.ListRemoteSessions(ctx)
		if err != nil {
			return failure("Could not find the source session.")
		}
		for _, candidate := range remotes {
			if candidate.AccountID == source.AccountID && candidate.ResumeSessionID == source.NativeSessionID && candidate.WorkspacePath == source.WorkspacePath {
				name = candidate.Name
				if err := s.claude.Deactivate(ctx, candidate.ID, false); err != nil {
					return failure("Wait for the source session to stop before continuing.")
				}
				if _, err := s.store.SetRemoteSessionDesired(ctx, candidate.ID, model.DesiredStopped); err != nil {
					return failure("Could not stop the source session.")
				}
			}
		}
		// Stopping the source can deliver its final checkpoint through a hook.
		source, err = s.store.GetSession(ctx, source.ID)
		if err != nil {
			return failure("Could not read the latest checkpoint.")
		}
		workspace, err := s.store.GetWorkspaceByPath(ctx, source.WorkspacePath)
		if err != nil {
			workspace, err = s.store.UpsertWorkspace(ctx, model.Workspace{Label: filepath.Base(source.WorkspacePath), Path: source.WorkspacePath})
			if err != nil {
				return failure("Could not prepare the destination workspace.")
			}
		}
		name = uniqueSessionName(truncate(name, 60), account.Email, workspace, remotes, slotID)
		selectionSource := model.RemoteSession{}
		if source.Provider == model.ProviderClaude {
			selectionSource = model.RemoteSession{AccountID: source.AccountID, WorkspacePath: source.WorkspacePath, ResumeSessionID: source.NativeSessionID}
		}
		settings, settingsErr := s.store.ResolveRemoteSelection(ctx, selectionSource, model.SelectionOverride{})
		if settingsErr != nil && !errors.Is(settingsErr, store.ErrNotFound) {
			return failure("Could not read the source agent settings.")
		}
		liveGit, err := checkpoint.NewGitCapturer().Capture(ctx, source.WorkspacePath)
		if err != nil {
			liveGit = model.GitSnapshot{}
		}
		contextText, err := checkpoint.NewService(s.store, nil).BuildContext(ctx, handoff, liveGit)
		if err != nil {
			return failure("Could not prepare the continuation checkpoint.")
		}
		if err := checkpoint.SaveContinuation(s.layout.Root, slotID, contextText); err != nil {
			return failure("Could not save the continuation checkpoint.")
		}
		remote, err = s.store.UpsertRemoteSession(ctx, model.RemoteSession{ID: slotID, Name: name, AccountID: account.ID,
			WorkspaceID: workspace.ID, WorkspacePath: workspace.Path, Model: settings.Model, Effort: settings.Effort, RequestedModel: settings.Model, RequestedEffort: settings.Effort,
			Desired: model.DesiredStopped})
		if err != nil {
			_ = checkpoint.RemoveContinuation(s.layout.Root, slotID)
			return failure("Could not create the continuation session.")
		}
	}
	if _, err := s.store.SetRemoteSessionDesired(ctx, slotID, model.DesiredRunning); err != nil {
		return failure("Could not start the continuation session.")
	}
	if err := s.startRemote(ctx, remote, false); err != nil {
		_, _ = s.store.SetRemoteSessionDesired(context.Background(), slotID, model.DesiredStopped)
		return failure("Could not start the destination. Your checkpoint is saved; try again.")
	}
	return handoff, "Claude", nil
}

func handoffInstructions(provider model.Provider, destination string) string {
	if provider == model.ProviderClaude {
		return "Continuation started with the checkpoint attached. Open the destination session to follow its progress."
	}
	return "Ready for 48 hours. Open " + destination + " in this workspace and ask it to continue."
}
