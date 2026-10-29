package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type managedWriteReceiptKey struct{}
type handoffEvidencePublicationKey struct{}
type replacementFreezeKey struct{}

func (s *Store) receiptReservation(ctx context.Context) (*gen.Queries, error) {
	q := s.projectDocumentReader(ctx)
	if ctx.Value(projectDocumentPublicationKey{}) != q {
		return nil, domain.ErrReplacementStateInvalid
	}
	return q, nil
}

// SaveProjectDocumentWrite mints an author receipt only after the managed write
// callback has published bytes under the authenticated writer reservation.
func (s *Store) SaveProjectDocumentWrite(ctx context.Context, id domain.ProjectID, state domain.ProjectDocumentState) (bool, error) {
	q, err := s.receiptReservation(ctx)
	if err != nil {
		return false, err
	}
	r, lease := state.LastHandoffWrite, state.Lease
	if r == nil || !replacementUUID(r.ID) || !replacementHash(r.Hash) || r.CreatedAt.IsZero() || lease == nil || lease.Frozen || lease.TokenHash == "" || r.SourceSessionID != lease.SessionID || r.Owner != lease.Owner || r.LeaseEpoch != lease.Epoch {
		return false, domain.ErrReplacementStateInvalid
	}
	return s.SaveProjectDocumentState(context.WithValue(ctx, managedWriteReceiptKey{}, q), id, state)
}

// SaveProjectDocumentEvidence records an immutable bundle's receipt inside its
// publication reservation. Generic ownership saves cannot create evidence.
func (s *Store) SaveProjectDocumentEvidence(ctx context.Context, id domain.ProjectID, state domain.ProjectDocumentState) (bool, error) {
	q, err := s.receiptReservation(ctx)
	if err != nil {
		return false, err
	}
	if state.Lease == nil || !state.Lease.Frozen {
		return false, domain.ErrReplacementStateInvalid
	}
	return s.SaveProjectDocumentState(context.WithValue(ctx, handoffEvidencePublicationKey{}, q), id, state)
}

// SaveReplacementFreeze binds the verified bytes and frozen writer together.
func (s *Store) SaveReplacementFreeze(ctx context.Context, op domain.OrchestratorReplacement) (bool, error) {
	q, err := s.receiptReservation(ctx)
	if err != nil {
		return false, err
	}
	state, err := s.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil {
		return false, err
	}
	p, lease, receipt := op.Preparation, state.Lease, state.LastHandoffWrite
	if op.Phase != domain.ReplacementExportingTranscript || op.FrozenAt == nil || op.Transcript == nil || p == nil || lease == nil || !lease.Frozen || lease.ReplacementID != op.ID || lease.Epoch != p.LeaseEpoch || lease.Owner != op.SourceOwner || receipt == nil || receipt.ID != p.WriteReceiptID || receipt.Hash != p.SubmittedHash {
		return false, domain.ErrReplacementStateInvalid
	}
	return s.SaveOrchestratorReplacement(context.WithValue(ctx, replacementFreezeKey{}, q), op)
}

// SaveReplacementEvidence binds only a receipt already committed by the actual
// bundle publication in this same transaction, never a caller-invented receipt.
func (s *Store) SaveReplacementEvidence(ctx context.Context, op domain.OrchestratorReplacement) (bool, error) {
	q, err := s.receiptReservation(ctx)
	if err != nil {
		return false, err
	}
	if op.Phase != domain.ReplacementTranscriptVerified || op.Transcript == nil || op.Transcript.Receipt == nil {
		return false, domain.ErrReplacementStateInvalid
	}
	state, err := s.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil {
		return false, err
	}
	if stored, ok := state.Evidence[op.ID+"/"+op.Transcript.AttemptID]; !ok || stored != *op.Transcript.Receipt {
		return false, domain.ErrReplacementStateInvalid
	}
	return s.SaveOrchestratorReplacement(context.WithValue(ctx, handoffEvidencePublicationKey{}, q), op)
}

func frozenHandoffHash(op domain.OrchestratorReplacement) string {
	if op.Transcript == nil {
		return ""
	}
	sum := sha256.Sum256(op.Transcript.FrozenHandoff)
	return hex.EncodeToString(sum[:])
}

func decodeDocumentState(raw string) (domain.ProjectDocumentState, error) {
	var state domain.ProjectDocumentState
	err := json.Unmarshal([]byte(raw), &state)
	return state, err
}
