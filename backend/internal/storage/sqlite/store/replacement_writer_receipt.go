package store

import (
	"context"
	"encoding/json"
	"math"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func validateReplacementWriterActivation(op domain.OrchestratorReplacement) error {
	w := op.WriterActivation
	if w == nil {
		if len(op.Notifications) != 0 {
			return domain.ErrReplacementStateInvalid
		}
		return nil
	}
	p := op.ResumeEvidence
	if op.SourceRetiredAt == nil || op.Preparation == nil || p == nil || p.StartedAt.IsZero() || p.ResumedAt == nil || p.VerifiedAt == nil || p.Probe == nil || p.Probe.SettledAt == nil || w.BeforeEpoch <= 0 || w.BeforeEpoch == math.MaxInt64 || w.BeforeEpoch != op.Preparation.LeaseEpoch || w.AfterEpoch != w.BeforeEpoch+1 || w.Owner != p.AfterOwner || w.CapabilityVerifier != p.AfterCapabilityVerifier || w.ActivatedAt.IsZero() {
		return domain.ErrReplacementStateInvalid
	}
	switch op.Phase {
	case domain.ReplacementNotifying, domain.ReplacementRecoveryRequired, domain.ReplacementCompleted:
	default:
		return domain.ErrReplacementStateInvalid
	}
	seen := map[domain.SessionID]bool{}
	for _, notice := range op.Notifications {
		if notice.SessionID == "" || notice.SessionID == op.SourceSessionID || notice.SessionID == op.SuccessorSessionID || seen[notice.SessionID] || notice.ContactID != "handoff-notice:"+op.ID+":"+string(notice.SessionID) || notice.State != "pending" || notice.CreatedAt.IsZero() {
			return domain.ErrReplacementStateInvalid
		}
		seen[notice.SessionID] = true
	}
	if op.Phase == domain.ReplacementCompleted && (op.IntakeReleasedAt == nil || op.IntakeDrainRequestedAt == nil || len(op.Notifications) != 0) {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

func preserveReplacementWriterActivation(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	a, b := before.WriterActivation, after.WriterActivation
	if a == nil {
		if b == nil {
			return nil
		}
		if ctx.Value(projectDocumentPublicationKey{}) != q || after.Phase != domain.ReplacementNotifying {
			return domain.ErrReplacementStateInvalid
		}
		row, err := q.GetProjectDocumentState(ctx, string(after.ProjectID))
		if err != nil {
			return err
		}
		var state domain.ProjectDocumentState
		if err := json.Unmarshal([]byte(row.StateJson), &state); err != nil {
			return err
		}
		lease := state.Lease
		if lease == nil || lease.Frozen || lease.SessionID != after.SuccessorSessionID || lease.Owner != b.Owner || lease.Epoch != b.AfterEpoch || lease.ClaimVerifier != b.CapabilityVerifier || lease.TokenHash != "" || lease.ReplacementID != after.ID {
			return domain.ErrReplacementStateInvalid
		}
		return nil
	}
	if b == nil || a.BeforeEpoch != b.BeforeEpoch || a.AfterEpoch != b.AfterEpoch || a.Owner != b.Owner || a.CapabilityVerifier != b.CapabilityVerifier || !a.ActivatedAt.Equal(b.ActivatedAt) {
		return domain.ErrReplacementStateInvalid
	}
	nextByID := make(map[domain.SessionID]domain.ReplacementWorkerNotification, len(after.Notifications))
	for _, notice := range after.Notifications {
		nextByID[notice.SessionID] = notice
	}
	for _, old := range before.Notifications {
		next, found := nextByID[old.SessionID]
		if !found || next.ContactID != old.ContactID || next.ObservedOwner != old.ObservedOwner || !next.CreatedAt.Equal(old.CreatedAt) {
			return domain.ErrReplacementStateInvalid
		}
	}
	return nil
}
