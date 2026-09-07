package store

import (
	"context"
	"database/sql"
	"fmt"
)

type migration struct {
	version    int
	statements []string
}

var migrations = []migration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS accounts (
				id TEXT PRIMARY KEY,
				email TEXT NOT NULL,
				profile_dir TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL,
				active INTEGER NOT NULL DEFAULT 0 CHECK (active IN (0, 1)),
				last_error TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS workspaces (
				id TEXT PRIMARY KEY,
				label TEXT NOT NULL,
				path TEXT NOT NULL UNIQUE,
				selected INTEGER NOT NULL DEFAULT 0 CHECK (selected IN (0, 1)),
				created_at TEXT NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS workspaces_one_selected
				ON workspaces(selected) WHERE selected = 1`,
			`CREATE TABLE IF NOT EXISTS sessions (
				id TEXT PRIMARY KEY,
				provider TEXT NOT NULL CHECK (provider IN ('claude', 'codex')),
				native_session_id TEXT NOT NULL,
				account_id TEXT NOT NULL DEFAULT '',
				title TEXT NOT NULL DEFAULT '',
				workspace_path TEXT NOT NULL DEFAULT '',
				worktree_path TEXT NOT NULL DEFAULT '',
				branch TEXT NOT NULL DEFAULT '',
				head_sha TEXT NOT NULL DEFAULT '',
				state TEXT NOT NULL,
				last_prompt TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL,
				UNIQUE(provider, native_session_id)
			)`,
			`CREATE INDEX IF NOT EXISTS sessions_workspace_updated
				ON sessions(workspace_path, updated_at DESC)`,
			`CREATE TABLE IF NOT EXISTS turns (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
				native_turn_id TEXT NOT NULL,
				prompt TEXT NOT NULL DEFAULT '',
				assistant_message TEXT NOT NULL DEFAULT '',
				state TEXT NOT NULL,
				git_json TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL,
				UNIQUE(session_id, native_turn_id)
			)`,
			`CREATE INDEX IF NOT EXISTS turns_session_updated
				ON turns(session_id, updated_at DESC)`,
			`CREATE TABLE IF NOT EXISTS handoffs (
				id TEXT PRIMARY KEY,
				source_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
				destination_provider TEXT NOT NULL CHECK (destination_provider IN ('claude', 'codex')),
				destination_account_id TEXT NOT NULL DEFAULT '',
				workspace_path TEXT NOT NULL,
				expires_at TEXT NOT NULL,
				consumed_at TEXT,
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS handoffs_pending_destination
				ON handoffs(destination_provider, destination_account_id, workspace_path, created_at DESC)
				WHERE consumed_at IS NULL`,
		},
	},
	{
		version: 2,
		statements: []string{
			`ALTER TABLE handoffs ADD COLUMN claim_token TEXT`,
			`ALTER TABLE handoffs ADD COLUMN claimed_at TEXT`,
			`CREATE INDEX IF NOT EXISTS handoffs_claimed_at ON handoffs(claimed_at)`,
		},
	},
	{
		version: 3,
		statements: []string{
			`CREATE UNIQUE INDEX IF NOT EXISTS accounts_one_active ON accounts(active) WHERE active = 1`,
		},
	},
	{
		version: 4,
		statements: []string{
			`ALTER TABLE sessions ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0, 1))`,
		},
	},
	{
		version: 5,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS remote_sessions (
				id TEXT PRIMARY KEY,
				name TEXT NOT NULL DEFAULT '',
				account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
				workspace_id TEXT NOT NULL DEFAULT '',
				workspace_path TEXT NOT NULL,
				resume_session_id TEXT NOT NULL DEFAULT '',
				desired TEXT NOT NULL DEFAULT 'running' CHECK (desired IN ('running', 'stopped')),
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS remote_sessions_account
				ON remote_sessions(account_id, updated_at DESC)`,
			// Carry the single pre-upgrade routing (active account + selected
			// workspace) over as the first remote session so a live install keeps
			// its worker across the upgrade.
			`INSERT INTO remote_sessions (id, name, account_id, workspace_id, workspace_path, resume_session_id, desired, created_at, updated_at)
				SELECT 'rs_restored', 'Vibe Remote · ' || accounts.email || ' · ' || workspaces.label,
					accounts.id, workspaces.id, workspaces.path, '', 'running',
					strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z',
					strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'
				FROM accounts, workspaces
				WHERE accounts.active = 1 AND workspaces.selected = 1`,
			// Several remote sessions may now run at once, so a single active
			// account is no longer the routing rule.
			`DROP INDEX IF EXISTS accounts_one_active`,
			`UPDATE accounts SET active = 0 WHERE active = 1`,
		},
	},
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}

	for _, item := range migrations {
		applied, err := migrationApplied(ctx, s.db, item.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", item.version, err)
		}
		applied, err = migrationAppliedTx(ctx, tx, item.version)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if applied {
			_ = tx.Rollback()
			continue
		}
		for _, statement := range item.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply migration %d: %w", item.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, item.version, formatTime(s.now().UTC())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", item.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", item.version, err)
		}
	}
	return nil
}

func migrationAppliedTx(ctx context.Context, tx *sql.Tx, version int) (bool, error) {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&found)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("read migration %d in transaction: %w", version, err)
}

func migrationApplied(ctx context.Context, db *sql.DB, version int) (bool, error) {
	var found int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&found)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("read migration %d: %w", version, err)
}
