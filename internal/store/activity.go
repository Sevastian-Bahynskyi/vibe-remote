package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

type ActivityEvent struct {
	SlotID        string
	RunID         string
	SessionID     string
	AccountID     string
	WorkspacePath string
	Event         string
	RequestID     string
	Name          string
	OccurredAt    time.Time
}

var activityNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

func (s *Store) BeginWorkerActivity(ctx context.Context, slotID, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM worker_activity_runs WHERE slot_id = ?`, slotID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_activity_runs (slot_id, run_id, session_id) SELECT id, ?, resume_session_id FROM remote_sessions WHERE id = ?`, runID, slotID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) EndWorkerActivity(ctx context.Context, slotID, runID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE worker_activity_runs SET closed = 1 WHERE slot_id = ? AND run_id = ?`, slotID, runID)
	return err
}

func (s *Store) RecordWorkerActivity(ctx context.Context, event ActivityEvent) error {
	if event.RunID == "" || event.SlotID == "" || !model.ValidResumeSessionID(event.SessionID) {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sessionID string
	var accepting, closed int
	err = tx.QueryRowContext(ctx, `SELECT a.session_id, a.accepting, a.closed
		FROM worker_activity_runs a JOIN remote_sessions r ON r.id = a.slot_id
		WHERE a.slot_id = ? AND a.run_id = ? AND r.account_id = ? AND r.workspace_path = ?`,
		event.SlotID, event.RunID, event.AccountID, event.WorkspacePath).Scan(&sessionID, &accepting, &closed)
	if errors.Is(err, sql.ErrNoRows) || closed != 0 || (sessionID != "" && sessionID != event.SessionID) {
		return nil
	}
	if err != nil {
		return err
	}
	if sessionID == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE worker_activity_runs SET session_id = ? WHERE slot_id = ?`, event.SessionID, event.SlotID); err != nil {
			return err
		}
	}
	switch event.Event {
	case "SessionStart":
	case "UserPromptSubmit":
		if _, err := tx.ExecContext(ctx, `UPDATE worker_activity_runs SET accepting = 1 WHERE slot_id = ?`, event.SlotID); err != nil {
			return err
		}
	case "Stop", "StopFailure", "Interrupt", "SessionEnd":
		ended := 0
		if event.Event == "SessionEnd" {
			ended = 1
		}
		if _, err := tx.ExecContext(ctx, `UPDATE worker_activity_runs SET accepting = 0, closed = ? WHERE slot_id = ?`, ended, event.SlotID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE worker_activity_items SET finished = 1 WHERE slot_id = ?`, event.SlotID); err != nil {
			return err
		}
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionDenied", "Elicitation", "ElicitationResult":
		if event.RequestID == "" || len(event.RequestID) > 256 || !activityNamePattern.MatchString(event.Name) {
			return nil
		}
		kind := "tool"
		finished := 1
		if event.Event == "Elicitation" || event.Event == "ElicitationResult" {
			kind = "confirmation"
		}
		parentToolID := ""
		parentFinished := 0
		anonymous := 0
		if event.RequestID == "server:"+event.Name {
			anonymous = 1
		}
		if kind == "confirmation" {
			prefix := "mcp__" + event.Name + "__"
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(request_id), '') FROM worker_activity_items WHERE slot_id = ? AND kind = 'tool' AND finished = 0 AND substr(name, 1, length(?)) = ?`, event.SlotID, prefix, prefix).Scan(&count, &parentToolID); err != nil {
				return err
			}
			if count == 0 && anonymous != 0 {
				err := tx.QueryRowContext(ctx, `SELECT request_id, finished FROM worker_activity_items WHERE slot_id = ? AND kind = 'tool' AND substr(name, 1, length(?)) = ? ORDER BY started_at DESC LIMIT 1`, event.SlotID, prefix, prefix).Scan(&parentToolID, &parentFinished)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			} else if count != 1 {
				parentToolID = ""
			}
			if event.RequestID == "server:"+event.Name {
				event.RequestID += ":" + parentToolID
			}
		}
		if event.Event == "PreToolUse" || event.Event == "Elicitation" {
			if accepting == 0 {
				return nil
			}
			finished = 0
			if parentFinished != 0 {
				finished = 1
			}
		}
		occurredAt := event.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = s.now()
		}
		hash := sha256.Sum256([]byte(event.RequestID))
		requestHash := hex.EncodeToString(hash[:])
		if anonymous != 0 {
			var existingHash string
			err := tx.QueryRowContext(ctx, `SELECT request_id FROM worker_activity_items WHERE slot_id = ? AND kind = 'confirmation' AND name = ? AND anonymous = 1 AND (finished = 0 OR parent_tool_id = ?) ORDER BY finished, started_at DESC LIMIT 1`, event.SlotID, event.Name, parentToolID).Scan(&existingHash)
			if err == nil {
				requestHash = existingHash
				if event.Event == "ElicitationResult" && parentToolID != "" {
					if _, err := tx.ExecContext(ctx, `UPDATE worker_activity_items SET parent_tool_id = ? WHERE slot_id = ? AND kind = 'confirmation' AND request_id = ? AND parent_tool_id = ''`, parentToolID, event.SlotID, requestHash); err != nil {
						return err
					}
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO worker_activity_items (slot_id, kind, request_id, name, parent_tool_id, anonymous, started_at, finished)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (slot_id, kind, request_id) DO UPDATE SET finished = MAX(worker_activity_items.finished, excluded.finished)`,
			event.SlotID, kind, requestHash, event.Name, parentToolID, anonymous, formatTime(occurredAt.UTC()), finished)
		if err != nil {
			return err
		}
		if kind == "tool" {
			if suffix, ok := strings.CutPrefix(event.Name, "mcp__"); ok {
				server, _, ok := strings.Cut(suffix, "__")
				if ok {
					prefix := "mcp__" + server + "__"
					var pending int
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_activity_items WHERE slot_id = ? AND kind = 'tool' AND finished = 0 AND substr(name, 1, length(?)) = ?`, event.SlotID, prefix, prefix).Scan(&pending); err != nil {
						return err
					}
					if (finished == 0 && pending == 1) || (finished != 0 && pending == 0) {
						if _, err := tx.ExecContext(ctx, `UPDATE worker_activity_items SET parent_tool_id = ? WHERE slot_id = ? AND kind = 'confirmation' AND name = ? AND parent_tool_id = ''`, requestHash, event.SlotID, server); err != nil {
							return err
						}
					}
					if finished != 0 && pending == 1 {
						if _, err := tx.ExecContext(ctx, `UPDATE worker_activity_items SET parent_tool_id = (SELECT request_id FROM worker_activity_items WHERE slot_id = ? AND kind = 'tool' AND finished = 0 AND substr(name, 1, length(?)) = ? LIMIT 1) WHERE slot_id = ? AND kind = 'confirmation' AND name = ? AND anonymous = 1 AND finished = 1 AND parent_tool_id = ''`, event.SlotID, prefix, prefix, event.SlotID, server); err != nil {
							return err
						}
					}
				}
			}
			if finished != 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE worker_activity_items SET finished = 1 WHERE slot_id = ? AND kind = 'confirmation' AND parent_tool_id = ?`, event.SlotID, requestHash); err != nil {
					return err
				}
			}
		}
	default:
		return nil
	}
	return tx.Commit()
}

func (s *Store) WorkerActivity(ctx context.Context, slotID, runID string) (model.WorkerActivity, error) {
	activity := model.WorkerActivity{}
	rows, err := s.db.QueryContext(ctx, `SELECT i.kind, i.name, i.started_at FROM worker_activity_items i
		JOIN worker_activity_runs a ON a.slot_id = i.slot_id
		WHERE i.slot_id = ? AND a.run_id = ? AND a.closed = 0 AND i.finished = 0 ORDER BY i.started_at, i.kind, i.request_id`, slotID, runID)
	if err != nil {
		return activity, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, started string
		var item model.ActivityItem
		if err := rows.Scan(&kind, &item.Name, &started); err != nil {
			return activity, err
		}
		item.StartedAt, err = parseTime(started)
		if err != nil {
			return activity, err
		}
		if kind == "confirmation" {
			activity.Confirmations = append(activity.Confirmations, item)
		} else {
			activity.Tools = append(activity.Tools, item)
		}
	}
	return activity, rows.Err()
}
