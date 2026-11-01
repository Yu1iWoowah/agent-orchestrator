package domain

import "time"

// ChatWakeAttempt diagnoses background delivery without changing input outcome.
type ChatWakeAttempt struct {
	ID              string    `json:"id"`
	SessionID       SessionID `json:"sessionId"`
	OwnerGeneration string    `json:"ownerGeneration,omitempty"`
	Phase           string    `json:"phase" enum:"waking,draining,ready,blocked"`
	Version         int64     `json:"version"`
	ErrorCode       string    `json:"errorCode,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// Validate restricts durable wake diagnostics to bounded protocol fields.
func (a ChatWakeAttempt) Validate() error {
	if !journalID(a.ID) || !journalID(string(a.SessionID)) || a.OwnerGeneration != "" && !journalID(a.OwnerGeneration) || a.Version < 0 || a.CreatedAt.IsZero() || a.UpdatedAt.Before(a.CreatedAt) || !journalCode(a.ErrorCode) {
		return ErrInvalidChatJournal
	}
	switch a.Phase {
	case "waking", "draining", "ready", "blocked":
		return nil
	}
	return ErrInvalidChatJournal
}
