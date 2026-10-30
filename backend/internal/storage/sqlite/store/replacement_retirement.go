package store

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementRetirementKey struct{}

// CommitReplacementRetirement records the source's permanent retirement and
// the next operation phase atomically. The caller has confirmed shutdown and
// closed only source-scoped resources. finalCheck must be a bounded witness
// comparison, never a native transcript parse or provider action.
func (s *Store) CommitReplacementRetirement(ctx context.Context, operation string, revision int64, expected domain.SessionRecord, documentRevision int64, finalCheck func(context.Context) error) (domain.OrchestratorReplacement, bool, error) {
	var next domain.OrchestratorReplacement
	if finalCheck == nil {
		return next, false, domain.ErrReplacementStateInvalid
	}
	applied, err := s.WithReplacementDocumentPublication(ctx, operation, revision, expected, documentRevision, func(reserved context.Context) error {
		q := s.replacementReader(reserved)
		row, err := q.GetOrchestratorReplacement(reserved, operation)
		if err != nil {
			return err
		}
		op, err := decodeReplacement(row)
		if err != nil {
			return err
		}
		p, transcript := op.Preparation, op.Transcript
		if op.Phase != domain.ReplacementRetiring || op.SourceRetiredAt != nil || op.FrozenAt == nil || op.SourceStopIntentAt == nil || p == nil || p.VerifiedAt == nil || p.LeaseEpoch <= 0 || p.WriteReceiptID == "" || !replacementHash(p.SubmittedHash) || transcript == nil || transcript.CapturedAt == nil || transcript.Receipt == nil || !replacementHash(transcript.TranscriptHash) {
			return domain.ErrReplacementStateInvalid
		}
		source, err := q.GetSession(reserved, op.SourceSessionID)
		if err != nil {
			return err
		}
		rec := rowToRecord(source)
		if op.SourceStop == nil || !op.SourceStop.MatchesSource(rec) || rec.Metadata.RuntimeHandleID != expected.Metadata.RuntimeHandleID || op.IntakeRevision != p.IntakeRevision {
			return domain.ErrReplacementSourceChanged
		}
		state, err := s.GetProjectDocumentState(reserved, op.ProjectID)
		if err != nil {
			return err
		}
		lease, write := state.Lease, state.LastHandoffWrite
		if lease == nil || !lease.Frozen || lease.ReplacementID != op.ID || lease.SessionID != rec.ID || lease.Owner != op.SourceOwner || lease.Epoch != p.LeaseEpoch || lease.TokenHash == "" || write == nil || write.ID != p.WriteReceiptID || write.Hash != p.SubmittedHash || write.Path != op.DocumentPath || write.SourceSessionID != rec.ID || write.Owner != op.SourceOwner || write.CapabilityVerifier != op.SourceCapabilityVerifier || write.LeaseEpoch != p.LeaseEpoch {
			return domain.ErrReplacementStateInvalid
		}
		if receipt, found := state.Evidence[op.ID+"/"+transcript.AttemptID]; !found || receipt != *transcript.Receipt {
			return domain.ErrReplacementStateInvalid
		}
		if err := finalCheck(reserved); err != nil {
			return err
		}
		now := time.Now().UTC()
		count, err := q.RetireReplacementSession(reserved, gen.RetireReplacementSessionParams{SessionID: rec.ID, ExpectedRevision: rec.Revision, RetiredAt: now})
		if err != nil {
			return err
		}
		if count != 1 {
			return domain.ErrReplacementSourceChanged
		}
		op.SourceRetiredAt, op.UpdatedAt, op.Phase = &now, now, domain.ReplacementStarting
		op.Error, op.ResumePhase = nil, ""
		changed, err := s.SaveOrchestratorReplacement(context.WithValue(reserved, replacementRetirementKey{}, q), op)
		if err != nil {
			return err
		}
		if !changed {
			return domain.ErrReplacementSourceChanged
		}
		op.Revision++
		next = op
		return nil
	})
	if err != nil || !applied {
		return domain.OrchestratorReplacement{}, false, err
	}
	return next, true, nil
}
