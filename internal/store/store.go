package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type HookEventKind string

const (
	HookEventPrompt    HookEventKind = "prompt"
	HookEventStop      HookEventKind = "stop"
	HookEventComplete  HookEventKind = "complete"
	HookEventInterrupt HookEventKind = "interrupt"
	HookEventFailure   HookEventKind = "failure"
)

type HookEvent struct {
	Provider         model.Provider
	NativeSessionID  string
	NativeTurnID     string
	AccountID        string
	Title            string
	WorkspacePath    string
	WorktreePath     string
	Kind             HookEventKind
	Prompt           string
	AssistantMessage string
	Git              model.GitSnapshot
	OccurredAt       time.Time
}

type SessionFilter struct {
	Provider      model.Provider
	WorkspacePath string
	AccountID     string
	Limit         int
}

type CreateHandoffParams struct {
	SourceSessionID      string
	DestinationProvider  model.Provider
	DestinationAccountID string
	WorkspacePath        string
}

type HandoffClaim struct {
	Handoff model.Handoff
	Token   string
}

type Options struct {
	Now         func() time.Time
	BusyTimeout time.Duration
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(path string) (*Store, error) {
	return OpenWithOptions(path, Options{})
}

func OpenWithOptions(path string, options Options) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	if info, statErr := os.Lstat(absPath); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("database path must not be a symbolic link")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect database path: %w", statErr)
	}

	file, err := os.OpenFile(absPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure database file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close database file: %w", err)
	}

	busyTimeout := options.BusyTimeout
	if busyTimeout <= 0 {
		busyTimeout = 5 * time.Second
	}
	dsnURL := url.URL{Scheme: "file", Path: absPath}
	query := dsnURL.Query()
	query.Set("_busy_timeout", fmt.Sprintf("%d", busyTimeout.Milliseconds()))
	query.Set("_foreign_keys", "on")
	query.Set("_txlock", "immediate")
	dsnURL.RawQuery = query.Encode()
	dsn := dsnURL.String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	store := &Store{db: db, now: time.Now}
	if options.Now != nil {
		store.now = options.Now
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := enableWAL(db, busyTimeout); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(absPath, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure database file after migration: %w", err)
	}
	return store, nil
}

func enableWAL(db *sql.DB, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var mode string
		err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode)
		if err == nil && strings.EqualFold(mode, "wal") {
			return nil
		}
		if err == nil {
			return fmt.Errorf("enable WAL: SQLite returned journal mode %q", mode)
		}
		if !isBusy(err) || time.Now().After(deadline) {
			return fmt.Errorf("enable WAL: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func isBusy(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "sqlite_busy")
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) UpsertAccount(ctx context.Context, account model.Account) (model.Account, error) {
	if strings.TrimSpace(account.ID) == "" {
		account.ID = newID("acct")
	}
	if strings.TrimSpace(account.Email) == "" {
		return model.Account{}, errors.New("account email is required")
	}
	now := s.now().UTC()
	if account.CreatedAt.IsZero() {
		account.CreatedAt = now
	}
	account.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (id, email, profile_dir, status, active, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			email = excluded.email,
			profile_dir = excluded.profile_dir,
			status = excluded.status,
			active = excluded.active,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		account.ID, account.Email, account.ProfileDir, string(account.Status), account.Active,
		account.LastError, formatTime(account.CreatedAt), formatTime(account.UpdatedAt),
	)
	if err != nil {
		return model.Account{}, fmt.Errorf("upsert account: %w", err)
	}
	return s.GetAccount(ctx, account.ID)
}

func (s *Store) GetAccount(ctx context.Context, id string) (model.Account, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, email, profile_dir, status, active, last_error, created_at, updated_at
		FROM accounts WHERE id = ?`, id)
	return scanAccount(row)
}

func (s *Store) ListAccounts(ctx context.Context) ([]model.Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, email, profile_dir, status, active, last_error, created_at, updated_at
		FROM accounts ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	accounts := make([]model.Account, 0)
	for rows.Next() {
		account, scanErr := scanAccount(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list accounts rows: %w", err)
	}
	return accounts, nil
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "accounts", id)
}

func (s *Store) DeleteAccountAndSessions(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin account checkpoint deletion: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE provider = ? AND account_id = ?`, string(model.ProviderClaude), id); err != nil {
		return fmt.Errorf("delete account checkpoints: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account checkpoint deletion: %w", err)
	}
	return nil
}

func (s *Store) UpsertWorkspace(ctx context.Context, workspace model.Workspace) (model.Workspace, error) {
	workspace.Path = canonicalPath(strings.TrimSpace(workspace.Path))
	if workspace.Path == "." || workspace.Path == "" {
		return model.Workspace{}, errors.New("workspace path is required")
	}
	if strings.TrimSpace(workspace.ID) == "" {
		workspace.ID = newID("ws")
	}
	if strings.TrimSpace(workspace.Label) == "" {
		workspace.Label = filepath.Base(workspace.Path)
	}
	if workspace.CreatedAt.IsZero() {
		workspace.CreatedAt = s.now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Workspace{}, fmt.Errorf("begin workspace upsert: %w", err)
	}
	defer tx.Rollback()
	if workspace.Selected {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET selected = 0 WHERE selected = 1`); err != nil {
			return model.Workspace{}, fmt.Errorf("clear selected workspace: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workspaces (id, label, path, selected, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			label = excluded.label,
			path = excluded.path,
			selected = excluded.selected`,
		workspace.ID, workspace.Label, workspace.Path, workspace.Selected, formatTime(workspace.CreatedAt),
	)
	if err != nil {
		return model.Workspace{}, fmt.Errorf("upsert workspace: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Workspace{}, fmt.Errorf("commit workspace upsert: %w", err)
	}
	return s.GetWorkspace(ctx, workspace.ID)
}

func (s *Store) GetWorkspace(ctx context.Context, id string) (model.Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, label, path, selected, created_at FROM workspaces WHERE id = ?`, id)
	return scanWorkspace(row)
}

func (s *Store) GetWorkspaceByPath(ctx context.Context, path string) (model.Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, label, path, selected, created_at FROM workspaces WHERE path = ?`, canonicalPath(path))
	return scanWorkspace(row)
}

func (s *Store) ListWorkspaces(ctx context.Context) ([]model.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, label, path, selected, created_at FROM workspaces ORDER BY selected DESC, created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	workspaces := make([]model.Workspace, 0)
	for rows.Next() {
		workspace, scanErr := scanWorkspace(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		workspaces = append(workspaces, workspace)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workspace rows: %w", err)
	}
	return workspaces, nil
}

func (s *Store) DeleteWorkspace(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "workspaces", id)
}

func (s *Store) SetActiveRouting(ctx context.Context, accountID, workspaceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin active routing update: %w", err)
	}
	defer tx.Rollback()
	if err := requireRow(ctx, tx, "accounts", accountID); err != nil {
		return err
	}
	if err := requireRow(ctx, tx, "workspaces", workspaceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET active = 0 WHERE active = 1`); err != nil {
		return fmt.Errorf("clear active account: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET active = 1 WHERE id = ?`, accountID); err != nil {
		return fmt.Errorf("set active account: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET selected = 0 WHERE selected = 1`); err != nil {
		return fmt.Errorf("clear selected workspace: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET selected = 1 WHERE id = ?`, workspaceID); err != nil {
		return fmt.Errorf("set selected workspace: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit active routing update: %w", err)
	}
	return nil
}

func requireRow(ctx context.Context, tx *sql.Tx, table, id string) error {
	var query string
	switch table {
	case "accounts":
		query = `SELECT 1 FROM accounts WHERE id = ?`
	case "workspaces":
		query = `SELECT 1 FROM workspaces WHERE id = ?`
	default:
		return errors.New("invalid routing table")
	}
	var found int
	if err := tx.QueryRowContext(ctx, query, id).Scan(&found); err != nil {
		return normalizeScanError("load "+table+" record", err)
	}
	return nil
}

func (s *Store) UpsertSession(ctx context.Context, session model.Session) (model.Session, error) {
	if session.Provider != model.ProviderClaude && session.Provider != model.ProviderCodex {
		return model.Session{}, errors.New("valid session provider is required")
	}
	if strings.TrimSpace(session.NativeSessionID) == "" {
		return model.Session{}, errors.New("native session id is required")
	}
	if strings.TrimSpace(session.ID) == "" {
		session.ID = newID("sess")
	}
	if session.State == "" {
		session.State = model.SessionUnknown
	}
	if session.WorkspacePath != "" {
		session.WorkspacePath = canonicalPath(session.WorkspacePath)
	}
	if session.WorktreePath != "" {
		session.WorktreePath = canonicalPath(session.WorktreePath)
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = s.now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			id, provider, native_session_id, account_id, title, workspace_path, worktree_path,
			branch, head_sha, state, pinned, last_prompt, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, native_session_id) DO UPDATE SET
			account_id = CASE WHEN excluded.account_id = '' THEN sessions.account_id ELSE excluded.account_id END,
			title = CASE WHEN excluded.title = '' THEN sessions.title ELSE excluded.title END,
			workspace_path = CASE WHEN excluded.workspace_path = '' THEN sessions.workspace_path ELSE excluded.workspace_path END,
			worktree_path = CASE WHEN excluded.worktree_path = '' THEN sessions.worktree_path ELSE excluded.worktree_path END,
			branch = CASE WHEN excluded.branch = '' THEN sessions.branch ELSE excluded.branch END,
			head_sha = CASE WHEN excluded.head_sha = '' THEN sessions.head_sha ELSE excluded.head_sha END,
			state = excluded.state,
			pinned = excluded.pinned,
			last_prompt = CASE WHEN excluded.last_prompt = '' THEN sessions.last_prompt ELSE excluded.last_prompt END,
			updated_at = excluded.updated_at`,
		session.ID, string(session.Provider), session.NativeSessionID, session.AccountID, session.Title,
		session.WorkspacePath, session.WorktreePath, session.Branch, session.HeadSHA, string(session.State),
		session.Pinned, session.LastPrompt, formatTime(session.UpdatedAt), formatTime(session.UpdatedAt),
	)
	if err != nil {
		return model.Session{}, fmt.Errorf("upsert session: %w", err)
	}
	return s.GetSessionByNativeID(ctx, session.Provider, session.NativeSessionID)
}

func (s *Store) GetSession(ctx context.Context, id string) (model.Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelect+` WHERE id = ?`, id)
	return scanSession(row)
}

func (s *Store) InterruptSession(ctx context.Context, id string, git model.GitSnapshot, occurredAt time.Time) error {
	if occurredAt.IsZero() {
		occurredAt = s.now().UTC()
	}
	gitJSON, err := json.Marshal(git)
	if err != nil {
		return fmt.Errorf("encode interrupted session Git state: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session interrupt: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE sessions SET state = ?, branch = CASE WHEN ? = '' THEN branch ELSE ? END,
			head_sha = CASE WHEN ? = '' THEN head_sha ELSE ? END, updated_at = ? WHERE id = ?`,
		string(model.SessionInterrupted), git.Branch, git.Branch, git.HeadSHA, git.HeadSHA, formatTime(occurredAt), id)
	if err != nil {
		return fmt.Errorf("interrupt session: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read interrupted session count: %w", err)
	}
	if changed == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE turns SET state = ?, git_json = ?, updated_at = ?
		WHERE id = (SELECT id FROM turns WHERE session_id = ? ORDER BY updated_at DESC, id DESC LIMIT 1)`,
		string(model.SessionInterrupted), string(gitJSON), formatTime(occurredAt), id); err != nil {
		return fmt.Errorf("interrupt latest turn: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session interrupt: %w", err)
	}
	return nil
}

func (s *Store) GetSessionByNativeID(ctx context.Context, provider model.Provider, nativeID string) (model.Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelect+` WHERE provider = ? AND native_session_id = ?`, string(provider), nativeID)
	return scanSession(row)
}

func (s *Store) ListSessions(ctx context.Context, filter SessionFilter) ([]model.Session, error) {
	query := sessionSelect + ` WHERE 1 = 1`
	args := make([]any, 0, 4)
	if filter.Provider != "" {
		query += ` AND provider = ?`
		args = append(args, string(filter.Provider))
	}
	if filter.WorkspacePath != "" {
		query += ` AND workspace_path = ?`
		args = append(args, canonicalPath(filter.WorkspacePath))
	}
	if filter.AccountID != "" {
		query += ` AND account_id = ?`
		args = append(args, filter.AccountID)
	}
	query += ` ORDER BY updated_at DESC, id ASC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	sessions := make([]model.Session, 0)
	for rows.Next() {
		session, scanErr := scanSession(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list session rows: %w", err)
	}
	return sessions, nil
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "sessions", id)
}

func (s *Store) UpdateSessionMetadata(ctx context.Context, id, title string, pinned bool) (model.Session, error) {
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > 120 {
		return model.Session{}, errors.New("session title must contain 1 to 120 characters")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET title = ?, pinned = ? WHERE id = ?`, title, pinned, id)
	if err != nil {
		return model.Session{}, fmt.Errorf("update session metadata: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return model.Session{}, fmt.Errorf("read session metadata update count: %w", err)
	}
	if changed == 0 {
		return model.Session{}, ErrNotFound
	}
	return s.GetSession(ctx, id)
}

func (s *Store) CleanupClosedSessions(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM sessions WHERE pinned = 0 AND state != ? AND updated_at < ?`,
		string(model.SessionPrompted), formatTime(before.UTC()))
	if err != nil {
		return 0, fmt.Errorf("clean old checkpoints: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read cleaned checkpoint count: %w", err)
	}
	return count, nil
}

func (s *Store) ListTurns(ctx context.Context, sessionID string, limit int) ([]model.Turn, error) {
	query := `
		SELECT id, session_id, native_turn_id, prompt, assistant_message, state, git_json, created_at, updated_at
		FROM turns WHERE session_id = ? ORDER BY updated_at DESC, created_at DESC, id DESC`
	args := []any{sessionID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list turns: %w", err)
	}
	defer rows.Close()

	turns := make([]model.Turn, 0)
	for rows.Next() {
		turn, scanErr := scanTurn(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list turn rows: %w", err)
	}
	return turns, nil
}

func (s *Store) RecordHookEvent(ctx context.Context, event HookEvent) (model.Session, model.Turn, error) {
	if event.Provider != model.ProviderClaude && event.Provider != model.ProviderCodex {
		return model.Session{}, model.Turn{}, errors.New("valid hook provider is required")
	}
	if strings.TrimSpace(event.NativeSessionID) == "" {
		return model.Session{}, model.Turn{}, errors.New("native session id is required")
	}
	if event.Kind != HookEventPrompt && event.Kind != HookEventStop && event.Kind != HookEventComplete &&
		event.Kind != HookEventInterrupt && event.Kind != HookEventFailure {
		return model.Session{}, model.Turn{}, errors.New("valid hook event kind is required")
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = s.now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	if event.WorkspacePath != "" {
		event.WorkspacePath = canonicalPath(event.WorkspacePath)
	}
	if event.WorktreePath != "" {
		event.WorktreePath = canonicalPath(event.WorktreePath)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Session{}, model.Turn{}, fmt.Errorf("begin hook event: %w", err)
	}
	defer tx.Rollback()

	session, err := getOrCreateSession(ctx, tx, event)
	if err != nil {
		return model.Session{}, model.Turn{}, err
	}
	nativeTurnID := strings.TrimSpace(event.NativeTurnID)
	if nativeTurnID == "" && event.Kind == HookEventPrompt {
		nativeTurnID, err = latestTurnMissingPromptID(ctx, tx, session.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return model.Session{}, model.Turn{}, err
		}
	} else if nativeTurnID == "" {
		nativeTurnID, err = latestOpenNativeTurnID(ctx, tx, session.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return model.Session{}, model.Turn{}, err
		}
	}
	if nativeTurnID == "" {
		nativeTurnID = newID("native-turn")
	}

	turn, err := getTurnByNativeID(ctx, tx, session.ID, nativeTurnID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return model.Session{}, model.Turn{}, err
	}
	if errors.Is(err, ErrNotFound) {
		turn = model.Turn{
			ID:           newID("turn"),
			SessionID:    session.ID,
			NativeTurnID: nativeTurnID,
			State:        model.SessionUnknown,
			CreatedAt:    event.OccurredAt,
			UpdatedAt:    event.OccurredAt,
		}
	}
	mergeTurnEvent(&turn, event)
	if err := writeTurn(ctx, tx, turn); err != nil {
		return model.Session{}, model.Turn{}, err
	}
	if err := refreshSessionFromTurns(ctx, tx, session.ID, event); err != nil {
		return model.Session{}, model.Turn{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Session{}, model.Turn{}, fmt.Errorf("commit hook event: %w", err)
	}

	session, err = s.GetSession(ctx, session.ID)
	if err != nil {
		return model.Session{}, model.Turn{}, err
	}
	turn, err = s.getTurn(ctx, turn.ID)
	if err != nil {
		return model.Session{}, model.Turn{}, err
	}
	return session, turn, nil
}

func (s *Store) CreateHandoff(ctx context.Context, params CreateHandoffParams) (model.Handoff, error) {
	if params.DestinationProvider != model.ProviderClaude && params.DestinationProvider != model.ProviderCodex {
		return model.Handoff{}, errors.New("valid destination provider is required")
	}
	if strings.TrimSpace(params.SourceSessionID) == "" {
		return model.Handoff{}, errors.New("source session id is required")
	}
	if strings.TrimSpace(params.WorkspacePath) == "" {
		return model.Handoff{}, errors.New("workspace path is required")
	}
	if _, err := s.GetSession(ctx, params.SourceSessionID); err != nil {
		return model.Handoff{}, err
	}
	now := s.now().UTC()
	handoff := model.Handoff{
		ID:                   newID("handoff"),
		SourceSessionID:      params.SourceSessionID,
		DestinationProvider:  params.DestinationProvider,
		DestinationAccountID: params.DestinationAccountID,
		WorkspacePath:        canonicalPath(params.WorkspacePath),
		ExpiresAt:            now.Add(48 * time.Hour),
		CreatedAt:            now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Handoff{}, fmt.Errorf("begin handoff creation: %w", err)
	}
	defer tx.Rollback()
	var liveClaims int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM handoffs
		WHERE destination_provider = ? AND destination_account_id = ? AND workspace_path = ?
			AND consumed_at IS NULL AND claim_token IS NOT NULL AND claimed_at >= ?`,
		string(handoff.DestinationProvider), handoff.DestinationAccountID, handoff.WorkspacePath,
		formatTime(now.Add(-30*time.Second))).Scan(&liveClaims); err != nil {
		return model.Handoff{}, fmt.Errorf("check active handoff delivery: %w", err)
	}
	if liveClaims > 0 {
		return model.Handoff{}, errors.New("a handoff is currently being delivered; try again shortly")
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE handoffs SET expires_at = ?, claim_token = NULL, claimed_at = NULL
		WHERE destination_provider = ? AND destination_account_id = ? AND workspace_path = ?
			AND consumed_at IS NULL AND (claim_token IS NULL OR claimed_at < ?)`,
		formatTime(now), string(handoff.DestinationProvider), handoff.DestinationAccountID, handoff.WorkspacePath,
		formatTime(now.Add(-30*time.Second))); err != nil {
		return model.Handoff{}, fmt.Errorf("supersede prior handoff: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO handoffs (
			id, source_session_id, destination_provider, destination_account_id,
			workspace_path, expires_at, consumed_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, NULL, ?)`,
		handoff.ID, handoff.SourceSessionID, string(handoff.DestinationProvider),
		handoff.DestinationAccountID, handoff.WorkspacePath, formatTime(handoff.ExpiresAt), formatTime(handoff.CreatedAt),
	)
	if err != nil {
		return model.Handoff{}, fmt.Errorf("create handoff: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Handoff{}, fmt.Errorf("commit handoff creation: %w", err)
	}
	return handoff, nil
}

func (s *Store) ConsumeHandoff(
	ctx context.Context,
	destinationProvider model.Provider,
	destinationAccountID string,
	workspacePath string,
) (model.Handoff, error) {
	handoff, err := s.FindHandoff(ctx, destinationProvider, destinationAccountID, workspacePath)
	if err != nil {
		return model.Handoff{}, err
	}
	return s.ConsumeHandoffByID(ctx, handoff.ID)
}

func (s *Store) FindHandoff(
	ctx context.Context,
	destinationProvider model.Provider,
	destinationAccountID string,
	workspacePath string,
) (model.Handoff, error) {
	now := s.now().UTC()
	row := s.db.QueryRowContext(ctx, `
		SELECT id, source_session_id, destination_provider, destination_account_id,
			workspace_path, expires_at, consumed_at, created_at
		FROM handoffs
		WHERE destination_provider = ?
			AND destination_account_id = ?
			AND workspace_path = ?
			AND consumed_at IS NULL
			AND expires_at > ?
			AND (claim_token IS NULL OR claimed_at < ?)
		ORDER BY created_at DESC, id DESC
		LIMIT 1`,
		string(destinationProvider), destinationAccountID, canonicalPath(workspacePath), formatTime(now), formatTime(now.Add(-30*time.Second)),
	)
	return scanHandoff(row)
}

func (s *Store) ClaimHandoffByID(ctx context.Context, id string) (HandoffClaim, error) {
	now := s.now().UTC()
	token := newID("claim")
	row := s.db.QueryRowContext(ctx, `
		UPDATE handoffs SET claim_token = ?, claimed_at = ?
		WHERE id = ?
			AND consumed_at IS NULL
			AND expires_at > ?
			AND (claim_token IS NULL OR claimed_at < ?)
		RETURNING id, source_session_id, destination_provider, destination_account_id,
			workspace_path, expires_at, consumed_at, created_at`,
		token, formatTime(now), id, formatTime(now), formatTime(now.Add(-30*time.Second)),
	)
	handoff, err := scanHandoff(row)
	if err != nil {
		return HandoffClaim{}, err
	}
	return HandoffClaim{Handoff: handoff, Token: token}, nil
}

func (s *Store) CompleteHandoffClaim(ctx context.Context, id, token string) (model.Handoff, error) {
	now := s.now().UTC()
	row := s.db.QueryRowContext(ctx, `
		UPDATE handoffs SET consumed_at = ?, claim_token = NULL, claimed_at = NULL
		WHERE id = ? AND claim_token = ? AND consumed_at IS NULL AND expires_at > ?
		RETURNING id, source_session_id, destination_provider, destination_account_id,
			workspace_path, expires_at, consumed_at, created_at`,
		formatTime(now), id, token, formatTime(now),
	)
	return scanHandoff(row)
}

func (s *Store) ReleaseHandoffClaim(ctx context.Context, id, token string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE handoffs SET claim_token = NULL, claimed_at = NULL
		WHERE id = ? AND claim_token = ? AND consumed_at IS NULL`, id, token)
	if err != nil {
		return fmt.Errorf("release handoff claim: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read released handoff count: %w", err)
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ConsumeHandoffByID(ctx context.Context, id string) (model.Handoff, error) {
	now := s.now().UTC()
	row := s.db.QueryRowContext(ctx, `
		UPDATE handoffs SET consumed_at = ?
		WHERE id = ? AND consumed_at IS NULL AND expires_at > ?
		RETURNING id, source_session_id, destination_provider, destination_account_id,
			workspace_path, expires_at, consumed_at, created_at`,
		formatTime(now), id, formatTime(now),
	)
	return scanHandoff(row)
}

func (s *Store) ListHandoffs(ctx context.Context, includeConsumed bool) ([]model.Handoff, error) {
	query := `
		SELECT id, source_session_id, destination_provider, destination_account_id,
			workspace_path, expires_at, consumed_at, created_at
		FROM handoffs`
	if !includeConsumed {
		query += ` WHERE consumed_at IS NULL AND expires_at > ?`
	}
	query += ` ORDER BY created_at DESC, id DESC`
	args := make([]any, 0, 1)
	if !includeConsumed {
		args = append(args, formatTime(s.now().UTC()))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list handoffs: %w", err)
	}
	defer rows.Close()

	handoffs := make([]model.Handoff, 0)
	for rows.Next() {
		handoff, scanErr := scanHandoff(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		handoffs = append(handoffs, handoff)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list handoff rows: %w", err)
	}
	return handoffs, nil
}

const sessionSelect = `
	SELECT id, provider, native_session_id, account_id, title, workspace_path, worktree_path,
		branch, head_sha, state, pinned, last_prompt, updated_at
	FROM sessions`

type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(row scanner) (model.Account, error) {
	var account model.Account
	var status string
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&account.ID, &account.Email, &account.ProfileDir, &status, &account.Active,
		&account.LastError, &createdAt, &updatedAt,
	); err != nil {
		return model.Account{}, normalizeScanError("scan account", err)
	}
	account.Status = model.AccountStatus(status)
	var err error
	account.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return model.Account{}, fmt.Errorf("parse account created time: %w", err)
	}
	account.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return model.Account{}, fmt.Errorf("parse account updated time: %w", err)
	}
	return account, nil
}

func scanWorkspace(row scanner) (model.Workspace, error) {
	var workspace model.Workspace
	var createdAt string
	if err := row.Scan(&workspace.ID, &workspace.Label, &workspace.Path, &workspace.Selected, &createdAt); err != nil {
		return model.Workspace{}, normalizeScanError("scan workspace", err)
	}
	var err error
	workspace.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return model.Workspace{}, fmt.Errorf("parse workspace created time: %w", err)
	}
	return workspace, nil
}

func scanSession(row scanner) (model.Session, error) {
	var session model.Session
	var provider string
	var state string
	var updatedAt string
	if err := row.Scan(
		&session.ID, &provider, &session.NativeSessionID, &session.AccountID, &session.Title,
		&session.WorkspacePath, &session.WorktreePath, &session.Branch, &session.HeadSHA,
		&state, &session.Pinned, &session.LastPrompt, &updatedAt,
	); err != nil {
		return model.Session{}, normalizeScanError("scan session", err)
	}
	session.Provider = model.Provider(provider)
	session.State = model.SessionState(state)
	var err error
	session.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return model.Session{}, fmt.Errorf("parse session updated time: %w", err)
	}
	return session, nil
}

func scanTurn(row scanner) (model.Turn, error) {
	var turn model.Turn
	var state string
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&turn.ID, &turn.SessionID, &turn.NativeTurnID, &turn.Prompt, &turn.AssistantMessage,
		&state, &turn.GitJSON, &createdAt, &updatedAt,
	); err != nil {
		return model.Turn{}, normalizeScanError("scan turn", err)
	}
	turn.State = model.SessionState(state)
	var err error
	turn.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return model.Turn{}, fmt.Errorf("parse turn created time: %w", err)
	}
	turn.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return model.Turn{}, fmt.Errorf("parse turn updated time: %w", err)
	}
	return turn, nil
}

func scanHandoff(row scanner) (model.Handoff, error) {
	var handoff model.Handoff
	var provider string
	var expiresAt string
	var consumedAt sql.NullString
	var createdAt string
	if err := row.Scan(
		&handoff.ID, &handoff.SourceSessionID, &provider, &handoff.DestinationAccountID,
		&handoff.WorkspacePath, &expiresAt, &consumedAt, &createdAt,
	); err != nil {
		return model.Handoff{}, normalizeScanError("scan handoff", err)
	}
	handoff.DestinationProvider = model.Provider(provider)
	var err error
	handoff.ExpiresAt, err = parseTime(expiresAt)
	if err != nil {
		return model.Handoff{}, fmt.Errorf("parse handoff expiry: %w", err)
	}
	handoff.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return model.Handoff{}, fmt.Errorf("parse handoff creation time: %w", err)
	}
	if consumedAt.Valid {
		parsed, parseErr := parseTime(consumedAt.String)
		if parseErr != nil {
			return model.Handoff{}, fmt.Errorf("parse handoff consumption time: %w", parseErr)
		}
		handoff.ConsumedAt = &parsed
	}
	return handoff, nil
}

func getOrCreateSession(ctx context.Context, tx *sql.Tx, event HookEvent) (model.Session, error) {
	row := tx.QueryRowContext(ctx, sessionSelect+` WHERE provider = ? AND native_session_id = ?`, string(event.Provider), event.NativeSessionID)
	session, err := scanSession(row)
	if err == nil {
		return session, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return model.Session{}, err
	}

	title := strings.TrimSpace(event.Title)
	if title == "" {
		title = titleFromPrompt(event.Prompt)
	}
	session = model.Session{
		ID:              newID("sess"),
		Provider:        event.Provider,
		NativeSessionID: event.NativeSessionID,
		AccountID:       event.AccountID,
		Title:           title,
		WorkspacePath:   event.WorkspacePath,
		WorktreePath:    event.WorktreePath,
		Branch:          event.Git.Branch,
		HeadSHA:         event.Git.HeadSHA,
		State:           model.SessionUnknown,
		UpdatedAt:       event.OccurredAt,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, provider, native_session_id, account_id, title, workspace_path, worktree_path,
			branch, head_sha, state, pinned, last_prompt, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)
		ON CONFLICT(provider, native_session_id) DO NOTHING`,
		session.ID, string(session.Provider), session.NativeSessionID, session.AccountID, session.Title,
		session.WorkspacePath, session.WorktreePath, session.Branch, session.HeadSHA,
		string(session.State), formatTime(event.OccurredAt), formatTime(event.OccurredAt),
	)
	if err != nil {
		return model.Session{}, fmt.Errorf("create hook session: %w", err)
	}
	row = tx.QueryRowContext(ctx, sessionSelect+` WHERE provider = ? AND native_session_id = ?`, string(event.Provider), event.NativeSessionID)
	return scanSession(row)
}

func latestOpenNativeTurnID(ctx context.Context, tx *sql.Tx, sessionID string) (string, error) {
	var nativeID string
	err := tx.QueryRowContext(ctx, `
		SELECT native_turn_id FROM turns
		WHERE session_id = ? AND state NOT IN (?, ?)
		ORDER BY updated_at DESC, created_at DESC, id DESC LIMIT 1`,
		sessionID, string(model.SessionCompleted), string(model.SessionInterrupted),
	).Scan(&nativeID)
	if err != nil {
		return "", normalizeScanError("find open turn", err)
	}
	return nativeID, nil
}

func latestTurnMissingPromptID(ctx context.Context, tx *sql.Tx, sessionID string) (string, error) {
	var nativeID string
	err := tx.QueryRowContext(ctx, `
		SELECT native_turn_id FROM turns
		WHERE session_id = ? AND prompt = ''
		ORDER BY updated_at DESC, created_at DESC, id DESC LIMIT 1`, sessionID,
	).Scan(&nativeID)
	if err != nil {
		return "", normalizeScanError("find turn missing prompt", err)
	}
	return nativeID, nil
}

func getTurnByNativeID(ctx context.Context, tx *sql.Tx, sessionID string, nativeTurnID string) (model.Turn, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, session_id, native_turn_id, prompt, assistant_message, state, git_json, created_at, updated_at
		FROM turns WHERE session_id = ? AND native_turn_id = ?`, sessionID, nativeTurnID)
	return scanTurn(row)
}

func (s *Store) getTurn(ctx context.Context, id string) (model.Turn, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, session_id, native_turn_id, prompt, assistant_message, state, git_json, created_at, updated_at
		FROM turns WHERE id = ?`, id)
	return scanTurn(row)
}

func mergeTurnEvent(turn *model.Turn, event HookEvent) {
	if event.Prompt != "" && (event.Kind == HookEventPrompt || turn.Prompt == "") {
		turn.Prompt = event.Prompt
	}
	if event.AssistantMessage != "" && (event.Kind == HookEventComplete || turn.AssistantMessage == "") {
		turn.AssistantMessage = event.AssistantMessage
	}
	incomingState := stateForEvent(event.Kind)
	if event.Kind == HookEventFailure && turn.State != model.SessionCompleted && turn.State != model.SessionInterrupted {
		turn.State = model.SessionUnknown
	} else if stateRank(incomingState) > stateRank(turn.State) ||
		(stateRank(incomingState) == stateRank(turn.State) && event.OccurredAt.After(turn.UpdatedAt)) {
		turn.State = incomingState
	}
	if !isEmptyGit(event.Git) {
		gitJSON, err := json.Marshal(event.Git)
		if err == nil {
			turn.GitJSON = string(gitJSON)
		}
	}
	if turn.CreatedAt.IsZero() || event.OccurredAt.Before(turn.CreatedAt) {
		turn.CreatedAt = event.OccurredAt
	}
	if event.OccurredAt.After(turn.UpdatedAt) || turn.UpdatedAt.IsZero() {
		turn.UpdatedAt = event.OccurredAt
	}
}

func writeTurn(ctx context.Context, tx *sql.Tx, turn model.Turn) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO turns (
			id, session_id, native_turn_id, prompt, assistant_message, state, git_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, native_turn_id) DO UPDATE SET
			prompt = excluded.prompt,
			assistant_message = excluded.assistant_message,
			state = excluded.state,
			git_json = excluded.git_json,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`,
		turn.ID, turn.SessionID, turn.NativeTurnID, turn.Prompt, turn.AssistantMessage,
		string(turn.State), turn.GitJSON, formatTime(turn.CreatedAt), formatTime(turn.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("write hook turn: %w", err)
	}
	return nil
}

func refreshSessionFromTurns(ctx context.Context, tx *sql.Tx, sessionID string, event HookEvent) error {
	var state string
	var prompt string
	var updatedAt string
	err := tx.QueryRowContext(ctx, `
		SELECT state, prompt, updated_at FROM turns
		WHERE session_id = ?
		ORDER BY updated_at DESC, created_at DESC, id DESC LIMIT 1`, sessionID,
	).Scan(&state, &prompt, &updatedAt)
	if err != nil {
		return normalizeScanError("read latest session turn", err)
	}
	title := strings.TrimSpace(event.Title)
	if title == "" {
		title = titleFromPrompt(prompt)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE sessions SET
			account_id = CASE WHEN ? = '' THEN account_id ELSE ? END,
			title = CASE WHEN title = '' THEN ? ELSE title END,
			workspace_path = CASE WHEN ? = '' THEN workspace_path ELSE ? END,
			worktree_path = CASE WHEN ? = '' THEN worktree_path ELSE ? END,
			branch = CASE WHEN ? = '' THEN branch ELSE ? END,
			head_sha = CASE WHEN ? = '' THEN head_sha ELSE ? END,
			state = ?, last_prompt = ?, updated_at = ?
		WHERE id = ?`,
		event.AccountID, event.AccountID, title,
		event.WorkspacePath, event.WorkspacePath, event.WorktreePath, event.WorktreePath,
		event.Git.Branch, event.Git.Branch, event.Git.HeadSHA, event.Git.HeadSHA,
		state, prompt, updatedAt, sessionID,
	)
	if err != nil {
		return fmt.Errorf("refresh hook session: %w", err)
	}
	return nil
}

func stateForEvent(kind HookEventKind) model.SessionState {
	switch kind {
	case HookEventPrompt:
		return model.SessionPrompted
	case HookEventStop:
		return model.SessionStopped
	case HookEventComplete:
		return model.SessionCompleted
	case HookEventInterrupt:
		return model.SessionInterrupted
	case HookEventFailure:
		return model.SessionUnknown
	default:
		return model.SessionUnknown
	}
}

func stateRank(state model.SessionState) int {
	switch state {
	case model.SessionCompleted, model.SessionInterrupted:
		return 4
	case model.SessionStopped:
		return 3
	case model.SessionPrompted:
		return 2
	default:
		return 1
	}
}

func isEmptyGit(snapshot model.GitSnapshot) bool {
	return snapshot.Root == "" && snapshot.Branch == "" && snapshot.HeadSHA == "" &&
		snapshot.Status == "" && len(snapshot.ChangedPaths) == 0 && snapshot.CapturedAt.IsZero()
}

func titleFromPrompt(prompt string) string {
	title := strings.TrimSpace(prompt)
	if index := strings.IndexByte(title, '\n'); index >= 0 {
		title = title[:index]
	}
	const maxTitleBytes = 120
	if len(title) > maxTitleBytes {
		title = title[:maxTitleBytes]
	}
	return title
}

func deleteByID(ctx context.Context, db *sql.DB, table string, id string) error {
	result, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete %s row: %w", table, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read deleted %s count: %w", table, err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func normalizeScanError(operation string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", operation, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func newID(prefix string) string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic(fmt.Sprintf("generate id: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(bytes)
}

func canonicalPath(path string) string {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "." || cleaned == "" {
		return cleaned
	}
	absolute, err := filepath.Abs(cleaned)
	if err == nil {
		cleaned = absolute
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err == nil {
		return resolved
	}
	return cleaned
}
