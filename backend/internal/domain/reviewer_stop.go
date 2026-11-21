package domain

import (
	"errors"
	"time"
)

// ErrReviewerStopPending protects the last evidence for a reviewer stop.
var ErrReviewerStopPending = errors.New("reviewer stop is awaiting evidence")

// ReviewerStopState describes whether the original lineage was fully captured.
type ReviewerStopState string

// Reviewer stop capture states.
const (
	ReviewerStopCapturing ReviewerStopState = "capturing"
	ReviewerStopSealed    ReviewerStopState = "sealed"
	ReviewerStopUnknown   ReviewerStopState = "unknown"
)

// ReviewerStopKey identifies the original review and launch.
type ReviewerStopKey struct {
	ReviewID string
	LaunchID string
}

// ReviewerStopReceipt holds the execution evidence needed after a restart.
type ReviewerStopReceipt struct {
	ReviewerStopKey
	SessionID            SessionID
	HandleID             string
	InterfaceMode        ReviewerInterfaceMode
	ControllerGeneration string
	DataRoot             string
	BootID               string
	PIDNamespace         string
	ChatScope            *CompactionProcessScope
	State                ReviewerStopState
	Reason               string
	Processes            []ReviewerStopProcess
}

// ReviewerStopProcess identifies one member of a captured lineage.
type ReviewerStopProcess struct {
	PID       int
	Start     string
	StartedAt time.Time
}
