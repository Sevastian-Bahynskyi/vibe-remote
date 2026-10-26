package model

import "time"

type SetupState string

const (
	SetupPending     SetupState = "pending"
	SetupReady       SetupState = "ready"
	SetupCancelled   SetupState = "cancelled"
	SetupFailed      SetupState = "failed"
	SetupInterrupted SetupState = "interrupted"
)

type SetupJob struct {
	ID        string     `json:"id"`
	AccountID string     `json:"accountId,omitempty"`
	Email     string     `json:"email"`
	State     SetupState `json:"state"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}
