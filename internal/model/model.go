package model

import "time"

type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

type AccountStatus string

const (
	AccountPending       AccountStatus = "pending"
	AccountAuthenticated AccountStatus = "authenticated"
	AccountSignedOut     AccountStatus = "signed_out"
	AccountError         AccountStatus = "error"
)

type Account struct {
	ID         string        `json:"id"`
	Email      string        `json:"email"`
	ProfileDir string        `json:"-"`
	Status     AccountStatus `json:"status"`
	Active     bool          `json:"active"`
	LastError  string        `json:"lastError,omitempty"`
	CreatedAt  time.Time     `json:"createdAt"`
	UpdatedAt  time.Time     `json:"updatedAt"`
}

type Workspace struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Path      string    `json:"path"`
	Selected  bool      `json:"selected"`
	CreatedAt time.Time `json:"createdAt"`
}

type SessionState string

const (
	SessionPrompted    SessionState = "prompted"
	SessionStopped     SessionState = "stopped"
	SessionCompleted   SessionState = "completed"
	SessionInterrupted SessionState = "interrupted"
	SessionUnknown     SessionState = "unknown"
)

type Session struct {
	ID              string       `json:"id"`
	Provider        Provider     `json:"provider"`
	NativeSessionID string       `json:"nativeSessionId"`
	AccountID       string       `json:"accountId,omitempty"`
	Title           string       `json:"title"`
	WorkspacePath   string       `json:"workspacePath"`
	WorktreePath    string       `json:"worktreePath,omitempty"`
	Branch          string       `json:"branch,omitempty"`
	HeadSHA         string       `json:"headSha,omitempty"`
	State           SessionState `json:"state"`
	Pinned          bool         `json:"pinned"`
	LastPrompt      string       `json:"lastPrompt,omitempty"`
	UpdatedAt       time.Time    `json:"updatedAt"`
	ResumeCommand   string       `json:"resumeCommand,omitempty"`
	DesktopGuidance string       `json:"desktopGuidance,omitempty"`
	SharedWorktree  bool         `json:"sharedWorktree"`
}

type Turn struct {
	ID               string       `json:"id"`
	SessionID        string       `json:"sessionId"`
	NativeTurnID     string       `json:"nativeTurnId"`
	Prompt           string       `json:"prompt"`
	AssistantMessage string       `json:"assistantMessage,omitempty"`
	State            SessionState `json:"state"`
	GitJSON          string       `json:"-"`
	CreatedAt        time.Time    `json:"createdAt"`
	UpdatedAt        time.Time    `json:"updatedAt"`
}

type Handoff struct {
	ID                   string     `json:"id"`
	SourceSessionID      string     `json:"sourceSessionId"`
	DestinationProvider  Provider   `json:"destinationProvider"`
	DestinationAccountID string     `json:"destinationAccountId,omitempty"`
	WorkspacePath        string     `json:"workspacePath"`
	ExpiresAt            time.Time  `json:"expiresAt"`
	ConsumedAt           *time.Time `json:"consumedAt,omitempty"`
	CreatedAt            time.Time  `json:"createdAt"`
}

type GitSnapshot struct {
	Root         string    `json:"root,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	HeadSHA      string    `json:"headSha,omitempty"`
	Status       string    `json:"status,omitempty"`
	ChangedPaths []string  `json:"changedPaths,omitempty"`
	CapturedAt   time.Time `json:"capturedAt"`
}

type WorkerStatus struct {
	AccountID     string `json:"accountId,omitempty"`
	WorkspacePath string `json:"workspacePath,omitempty"`
	Running       bool   `json:"running"`
	PID           int    `json:"pid,omitempty"`
	State         string `json:"state"`
	LastError     string `json:"lastError,omitempty"`
}
