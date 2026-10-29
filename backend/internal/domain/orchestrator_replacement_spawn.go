package domain

import "time"

// ReplacementSpawnAttempt fences background preparation and distinguishes a
// proven prelaunch failure from an unknown provider launch outcome.
type ReplacementSpawnAttempt struct {
	ID              string     `json:"id"`
	RetryGeneration int64      `json:"retryGeneration"`
	StartedAt       time.Time  `json:"startedAt"`
	LaunchIntentAt  *time.Time `json:"launchIntentAt,omitempty"`
	FailedAt        *time.Time `json:"failedAt,omitempty"`
}

// LatestSpawnAttempt returns the most recently reserved preparation identity.
func (op OrchestratorReplacement) LatestSpawnAttempt() (ReplacementSpawnAttempt, bool) {
	if len(op.SpawnAttempts) == 0 {
		return ReplacementSpawnAttempt{}, false
	}
	return op.SpawnAttempts[len(op.SpawnAttempts)-1], true
}
