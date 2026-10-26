package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func optionalTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(value.UTC())
}

type settingsWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func writeAgentSettings(ctx context.Context, writer settingsWriter, provider model.Provider, nativeID string, settings AgentSettings, now time.Time) error {
	_, err := writer.ExecContext(ctx, `INSERT INTO session_agents(provider,native_session_id,model,effort,requested_model,requested_effort,observed_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(provider,native_session_id) DO UPDATE SET
 model=CASE WHEN excluded.model='' THEN session_agents.model ELSE excluded.model END,
 effort=CASE WHEN excluded.effort='' THEN session_agents.effort ELSE excluded.effort END,
 requested_model=CASE WHEN ? AND excluded.requested_model<>'' THEN excluded.requested_model ELSE session_agents.requested_model END,
 requested_effort=CASE WHEN ? AND excluded.requested_effort<>'' THEN excluded.requested_effort ELSE session_agents.requested_effort END,
 observed_at=excluded.observed_at,updated_at=excluded.updated_at`, provider, nativeID, settings.Model, settings.Effort,
		intentValue(settings.UpdateIntent, settings.RequestedModel), intentValue(settings.UpdateIntent, settings.RequestedEffort), formatTime(now), formatTime(now), settings.UpdateIntent, settings.UpdateIntent)
	return err
}

func intentValue(deliberate bool, value string) string {
	if deliberate {
		return value
	}
	return ""
}

func (s *Store) RecordManagedAgentSettings(ctx context.Context, event ActivityEvent, settings AgentSettings) (bool, error) {
	if event.RunID == "" || event.SlotID == "" {
		return false, nil
	}
	settings, err := validAgentSettings(settings)
	if err != nil {
		return false, err
	}
	if settings.RequestedModel != "" && !model.ValidClaudeModelName(settings.RequestedModel) {
		return false, errors.New("invalid requested Claude model")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var found int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM worker_activity_runs a JOIN remote_sessions r ON r.id=a.slot_id
 WHERE a.slot_id=? AND a.run_id=? AND a.closed=0 AND r.account_id=? AND r.workspace_path=? AND a.session_id=? AND r.resume_session_id=?`, event.SlotID, event.RunID, event.AccountID, event.WorkspacePath, event.SessionID, event.SessionID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := s.now().UTC()
	if err := writeAgentSettings(ctx, tx, model.ProviderClaude, event.SessionID, settings, now); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE remote_sessions SET model=CASE WHEN ?='' THEN model ELSE ? END, effort=CASE WHEN ?='' THEN effort ELSE ? END,
 requested_model=CASE WHEN ? AND ?<>'' THEN ? ELSE requested_model END,
 requested_effort=CASE WHEN ? AND ?<>'' THEN ? ELSE requested_effort END,observed_at=?,updated_at=? WHERE id=?`,
		settings.Model, settings.Model, settings.Effort, settings.Effort, settings.UpdateIntent, settings.RequestedModel, settings.RequestedModel,
		settings.UpdateIntent, settings.RequestedEffort, settings.RequestedEffort, formatTime(now), formatTime(now), event.SlotID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) ResolveRemoteSelection(ctx context.Context, remote model.RemoteSession, operation model.SelectionOverride) (model.AgentSelection, error) {
	conversation := model.AgentSelection{}
	if remote.ResumeSessionID != "" {
		settings, err := s.GetAgentSettings(ctx, model.ProviderClaude, remote.ResumeSessionID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return model.AgentSelection{}, err
		}
		conversation = model.AgentSelection{Model: settings.Model, Effort: settings.Effort, RequestedModel: settings.RequestedModel, RequestedEffort: settings.RequestedEffort}
	}
	fallback := model.AgentSelection{Model: remote.Model, Effort: remote.Effort, RequestedModel: remote.RequestedModel, RequestedEffort: remote.RequestedEffort}
	if fallback == (model.AgentSelection{}) && remote.ResumeSessionID != "" {
		slots, err := s.ListRemoteSessions(ctx)
		if err != nil {
			return model.AgentSelection{}, err
		}
		for _, candidate := range slots {
			if candidate.AccountID == remote.AccountID && candidate.WorkspacePath == remote.WorkspacePath && candidate.ResumeSessionID == remote.ResumeSessionID {
				fallback = model.AgentSelection{Model: candidate.Model, Effort: candidate.Effort, RequestedModel: candidate.RequestedModel, RequestedEffort: candidate.RequestedEffort}
				break
			}
		}
	}
	return model.ResolveClaudeSelection(operation, conversation, fallback)
}

func (s *Store) SetRemoteRequestedSelection(ctx context.Context, slotID string, selection model.AgentSelection) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE remote_sessions SET requested_model=?,requested_effort=?,model=CASE WHEN observed_at IS NULL THEN ? ELSE model END,effort=CASE WHEN observed_at IS NULL THEN ? ELSE effort END WHERE id=?`, selection.Model, selection.Effort, selection.Model, selection.Effort, slotID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_agents(provider,native_session_id,model,effort,requested_model,requested_effort,updated_at)
 SELECT 'claude',resume_session_id,'','',?,?,? FROM remote_sessions WHERE id=? AND resume_session_id<>''
 ON CONFLICT(provider,native_session_id) DO UPDATE SET requested_model=excluded.requested_model,requested_effort=excluded.requested_effort,updated_at=excluded.updated_at`, selection.Model, selection.Effort, formatTime(s.now().UTC()), slotID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
