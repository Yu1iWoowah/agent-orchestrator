package ports

import "errors"

// Agent settings persistence errors.
var (
	ErrAgentSettingsNotFound = errors.New("agent settings not found")
	ErrAgentSettingsConflict = errors.New("agent settings changed or another operation is pending")
)
