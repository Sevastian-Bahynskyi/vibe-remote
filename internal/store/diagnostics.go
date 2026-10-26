package store

import (
	"context"
	"time"
)

type Counters struct {
	ActivityRows           int64 `json:"activityRows"`
	UnfinishedActivityRows int64 `json:"unfinishedActivityRows"`
	AgentRows              int64 `json:"agentRows"`
}

func (s *Store) Counters(ctx context.Context) (Counters, error) {
	var value Counters
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM worker_activity_items),(SELECT COUNT(*) FROM worker_activity_items WHERE finished=0),(SELECT COUNT(*) FROM session_agents)`).Scan(&value.ActivityRows, &value.UnfinishedActivityRows, &value.AgentRows)
	return value, err
}

func (s *Store) CleanupOrphanMetadata(ctx context.Context, before time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM worker_activity_items WHERE EXISTS(SELECT 1 FROM worker_activity_runs a WHERE a.slot_id=worker_activity_items.slot_id AND a.closed=1)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_agents WHERE updated_at<?
 AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.provider=session_agents.provider AND s.native_session_id=session_agents.native_session_id)
 AND NOT EXISTS(SELECT 1 FROM remote_sessions r WHERE session_agents.provider='claude' AND r.resume_session_id=session_agents.native_session_id)`, formatTime(before.UTC())); err != nil {
		return err
	}
	return tx.Commit()
}
