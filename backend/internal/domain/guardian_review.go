package domain

import "time"

// GuardianReviewScope is captured by a controller before publishing its stream.
type GuardianReviewScope struct {
	SessionID    SessionID
	AccountID    string
	NativeRootID string
	Generation   string
	LaunchedAt   time.Time
}

// GuardianReview contains only provider metadata; EpochID scopes native IDs.
type GuardianReview struct {
	InvalidMetadata bool

	EpochID        int64
	ReviewID       string
	ParentThreadID string
	ParentTurnID   string
	TargetItemID   *string
	Action         string
	Status         string
	DecisionSource string
	StartedAt      *time.Time
	CompletedAt    *time.Time
	FirstSeenAt    time.Time
	RetentionAt    time.Time
	SawStarted     bool
	SawCompleted   bool
	Conflict       bool
}

// GuardianAction maps all unrecognized provider values to a fixed enum.
func GuardianAction(v string) string {
	switch v {
	case "command", "execve", "writeStdin", "applyPatch", "networkAccess", "mcpToolCall", "requestPermissions":
		return v
	}
	return "unknown"
}

// GuardianReviewStatus never retains arbitrary provider text.
func GuardianReviewStatus(v string) string {
	switch v {
	case "inProgress", "approved", "denied", "timedOut", "aborted":
		return v
	}
	return "unknown"
}

// GuardianDecisionSource is deliberately limited to the supported schema.
func GuardianDecisionSource(v string) string {
	if v == "agent" {
		return v
	}
	return "unknown"
}

// Valid rejects unsafe metadata before it enters the observation pipeline.
func (r GuardianReview) Valid() bool {
	return ValidGuardianID(r.ReviewID) && ValidGuardianID(r.ParentThreadID) && ValidGuardianID(r.ParentTurnID) &&
		(r.TargetItemID == nil || ValidGuardianID(*r.TargetItemID)) && r.Action == GuardianAction(r.Action) &&
		r.Status == GuardianReviewStatus(r.Status) && (r.DecisionSource == "" || r.DecisionSource == GuardianDecisionSource(r.DecisionSource)) &&
		(r.SawStarted || r.SawCompleted) && (r.StartedAt == nil || r.CompletedAt == nil || !r.CompletedAt.Before(*r.StartedAt))
}
