package codexappserver

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Only these primitive fields cross into the new observation pipeline. The
// existing action/detail objects are never copied or decoded by that pipeline.
func guardianReviewMetadata(thread, turn, id string, target *string, action, status, decision string, started, completed int64, done bool) *domain.GuardianReview {
	r := domain.GuardianReview{ReviewID: id, ParentThreadID: thread, ParentTurnID: turn, TargetItemID: target, Action: domain.GuardianAction(action), Status: domain.GuardianReviewStatus(status), SawStarted: !done, SawCompleted: done}
	if done {
		r.DecisionSource = domain.GuardianDecisionSource(decision)
	}
	if started > 0 {
		at := time.UnixMilli(started).UTC()
		r.StartedAt = &at
	}
	if completed > 0 {
		at := time.UnixMilli(completed).UTC()
		r.CompletedAt = &at
	}
	if !r.Valid() {
		return &domain.GuardianReview{InvalidMetadata: true}
	}
	return &r
}
