package store

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func (s *Store) CreateSetupJob(ctx context.Context, email, accountID string) (model.SetupJob, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || address.Name != "" {
		return model.SetupJob{}, errors.New("a valid email is required")
	}
	id := newID("setup")
	now := formatTime(s.now().UTC())
	_, err = s.db.ExecContext(ctx, `INSERT INTO setup_jobs(id,account_id,email,state,error,created_at,updated_at) VALUES(?,?,?,'pending','',?,?)`, id, accountID, email, now, now)
	if err != nil {
		return model.SetupJob{}, errors.New("account setup could not start; check for an existing pending sign-in")
	}
	return s.GetSetupJob(ctx, id)
}

func scanSetupJob(row scanner) (model.SetupJob, error) {
	var job model.SetupJob
	var created, updated string
	if err := row.Scan(&job.ID, &job.AccountID, &job.Email, &job.State, &job.Error, &created, &updated); err != nil {
		return job, normalizeScanError("get setup status", err)
	}
	var err error
	job.CreatedAt, err = parseTime(created)
	if err != nil {
		return job, err
	}
	job.UpdatedAt, err = parseTime(updated)
	return job, err
}

func (s *Store) GetSetupJob(ctx context.Context, id string) (model.SetupJob, error) {
	return scanSetupJob(s.db.QueryRowContext(ctx, `SELECT id,account_id,email,state,error,created_at,updated_at FROM setup_jobs WHERE id=?`, id))
}

func (s *Store) ListSetupJobs(ctx context.Context) ([]model.SetupJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,account_id,email,state,error,created_at,updated_at FROM setup_jobs ORDER BY created_at DESC LIMIT 30`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []model.SetupJob{}
	for rows.Next() {
		job, err := scanSetupJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) FinishSetupJob(ctx context.Context, id, accountID string, state model.SetupState) error {
	message := ""
	switch state {
	case model.SetupReady:
	case model.SetupCancelled:
		message = "Sign-in was cancelled. Retry when ready."
	case model.SetupFailed:
		message = "Setup did not complete. Check sign-in and repair the account."
	case model.SetupInterrupted:
		message = "Setup was interrupted. Retry to complete it."
	default:
		return errors.New("invalid setup state")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE setup_jobs SET state=?,account_id=CASE WHEN ?='' THEN account_id ELSE ? END,error=?,updated_at=? WHERE id=? AND state='pending'`, state, accountID, accountID, message, formatTime(s.now().UTC()), id)
	return err
}

func (s *Store) TouchSetupJob(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE setup_jobs SET updated_at=? WHERE id=? AND state='pending'`, formatTime(s.now().UTC()), id)
	return err
}

func (s *Store) InterruptStaleSetupJobs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE setup_jobs SET state='interrupted',error='Setup was interrupted. Retry to complete it.',updated_at=? WHERE state='pending' AND updated_at<?`, formatTime(s.now().UTC()), formatTime(s.now().Add(-30*time.Second).UTC()))
	return err
}
