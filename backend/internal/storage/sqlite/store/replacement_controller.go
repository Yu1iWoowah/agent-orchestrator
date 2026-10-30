package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// Called only in the same transaction as the native owner/branch publication.
func (s *Store) bindReplacementSuccessorController(ctx context.Context, q *gen.Queries, before, ready domain.SessionRecord, publishReady bool) error {
	row, err := q.GetActiveOrchestratorReplacement(ctx, string(ready.ProjectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if ready.Kind != domain.KindOrchestrator || ready.ID == domain.SessionID(row.SourceSessionID) {
		return nil
	}
	// Route unrelated controllers using sealed identity, never an unverified
	// successorSessionId from the mutable row. Corruption must remain fenced
	// without blocking ordinary Chat commits elsewhere in the project.
	checkpoint, err := q.GetLatestReplacementRepairSnapshot(ctx, row.ID)
	if err == nil {
		sealed, err := checkpointRow(checkpoint)
		if err != nil {
			return err
		}
		trusted, err := decodeReplacement(sealed)
		if err != nil {
			return err
		}
		if trusted.SuccessorSessionID != ready.ID {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	op, err := s.decodeCheckpointedReplacement(ctx, q, row)
	if err != nil {
		return err
	}
	if op.SuccessorSessionID != ready.ID {
		return nil
	}
	if op.SuccessorController != nil {
		if p := op.ResumeEvidence; publishReady && p != nil && p.ResumeIntentAt != nil && p.ResumedAt == nil && (op.Phase != domain.ReplacementVerifyingResume || ready.Metadata.ControllerGeneration != p.ResumeGeneration || before.Metadata.BrowserCapabilityVerifier != ready.Metadata.BrowserCapabilityVerifier) {
			return domain.ErrReplacementSourceChanged
		}

		if publishReady && op.ActivatedSuccessorMatches(ready) && (ready.ControllerOwner() != op.WriterActivation.Owner || ready.Metadata.BrowserCapabilityVerifier != op.WriterActivation.CapabilityVerifier) {
			return rebindActivatedReplacementWriter(ctx, q, op, before, ready)
		}
		return nil
	} // A later epoch cannot rewrite the first receipt.
	if op.Phase != domain.ReplacementStarting || op.SourceRetiredAt == nil || before.ClientRequestID == "" || before.ClientRequestID != op.SpawnRequestID || before.ClientRequestHash != op.SpawnRequestHash || before.Metadata.ProviderConversationID != "" || ready.Metadata.BrowserCapabilityVerifier == "" || ready.Metadata.BrowserCapabilityVerifier != before.Metadata.BrowserCapabilityVerifier || ready.Metadata.Model != op.TargetAgentConfig.Model || ready.Metadata.Effort != op.TargetAgentConfig.Effort || ready.Metadata.Permissions != op.TargetAgentConfig.Permissions {
		return domain.ErrReplacementSourceChanged
	}
	if matches, err := replacementProjectMatches(ctx, q, op); err != nil {
		return err
	} else if !matches {
		return domain.ErrReplacementConfigChanged
	}
	op.SuccessorController = &domain.ReplacementSuccessorController{Owner: ready.ControllerOwner(), CapabilityVerifier: ready.Metadata.BrowserCapabilityVerifier, CommittedAt: ready.UpdatedAt}
	if err := validateReplacementSuccessorController(op); err != nil {
		return err
	}
	op.UpdatedAt = ready.UpdatedAt
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	count, err := q.UpdateOrchestratorReplacement(ctx, gen.UpdateOrchestratorReplacementParams{ID: op.ID, Revision: op.Revision, Phase: string(op.Phase), IsActive: true, StateJson: string(data), UpdatedAt: op.UpdatedAt.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	if count != 1 {
		return domain.ErrReplacementSourceChanged
	}
	return s.saveReplacementCheckpoint(replacementRecoveryCheckpointContext(ctx, q), q, op.ID)
}

// CommitChatControllerReady atomically publishes a connected controller and
// its same-native writer continuation. Generic UpdateSession cannot rebind.
func (s *Store) CommitChatControllerReady(ctx context.Context, ready domain.SessionRecord) error {
	return s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetSession(ctx, ready.ID)
		if err != nil {
			return err
		}
		if err := q.UpdateSession(ctx, recordToUpdate(ready)); err != nil {
			return err
		}
		return s.bindReplacementSuccessorController(ctx, q, rowToRecord(row), ready, true)
	})
}

// ReconnectChatControllerReady verifies the already-claimed live controller
// without rewriting activity, recency, signal receipts or the session revision.
func (s *Store) ReconnectChatControllerReady(ctx context.Context, expected domain.SessionRecord) error {
	return s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetSession(ctx, expected.ID)
		if err != nil {
			return err
		}
		current := rowToRecord(row)
		if current.IsTerminated || current.ControllerOwner() != expected.ControllerOwner() || current.Metadata.BrowserCapabilityVerifier != expected.Metadata.BrowserCapabilityVerifier {
			return domain.ErrReplacementSourceChanged
		}
		return s.bindReplacementSuccessorController(ctx, q, current, current, true)
	})
}

func rebindActivatedReplacementWriter(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, before, ready domain.SessionRecord) error {
	if before.ControllerOwner() != ready.ControllerOwner() || before.Metadata.BrowserCapabilityVerifier != ready.Metadata.BrowserCapabilityVerifier || ready.ProvisionState != domain.SessionProvisionReady {
		return domain.ErrReplacementSourceChanged
	}
	row, err := q.GetProjectDocumentState(ctx, string(op.ProjectID))
	if err != nil {
		return err
	}
	state, err := decodeDocumentState(row.StateJson)
	if err != nil {
		return err
	}
	project, err := q.GetProject(ctx, op.ProjectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt.Valid || state.SharedDir != op.SharedDir {
		return domain.ErrReplacementConfigChanged
	}
	matched, err := projectDocumentWriterMatches(ctx, q, op.ProjectID, ready, row.Revision, false)
	if err != nil {
		return err
	}
	if !matched {
		return domain.ErrReplacementSourceChanged
	}
	lease := state.Lease
	if lease == nil || lease.Frozen || lease.ReplacementID != op.ID || lease.SessionID != ready.ID || lease.Epoch != op.WriterActivation.AfterEpoch || lease.Owner.ProviderConversationID != ready.Metadata.ProviderConversationID {
		return domain.ErrReplacementIntakeFenced
	}
	if lease.CapabilityVerifier == "" && lease.Owner != op.WriterActivation.Owner {
		return domain.ErrReplacementIntakeFenced
	}
	if lease.Owner == ready.ControllerOwner() && replacementLeaseCapability(op, lease) == ready.Metadata.BrowserCapabilityVerifier && (lease.ClaimVerifier == ready.Metadata.BrowserCapabilityVerifier || lease.ClaimVerifier == "" && lease.TokenHash != "") {
		return nil
	}
	lease.Owner = ready.ControllerOwner()
	lease.CapabilityVerifier = ready.Metadata.BrowserCapabilityVerifier
	lease.TokenHash, lease.ClaimVerifier = "", ready.Metadata.BrowserCapabilityVerifier
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	count, err := q.UpdateProjectDocumentState(ctx, gen.UpdateProjectDocumentStateParams{ProjectID: string(op.ProjectID), Revision: row.Revision, StateJson: string(data)})
	if err != nil {
		return err
	}
	if count != 1 {
		return domain.ErrReplacementSourceChanged
	}
	return nil
}

// ReplacementActivatedOwner verifies the current writer independently of the
// intake-release marker. Hibernated owners may use it to start the same native
// conversation; ordinary message dispatch still requires released_sessions.
func (s *Store) ReplacementActivatedOwner(ctx context.Context, id string, expected domain.SessionRecord) (bool, error) {
	matches := false
	err := s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(ctx, id)
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(ctx, q, row)
		if err != nil {
			return err
		}
		matches, err = replacementActivatedOwnerMatches(ctx, q, op, expected)
		return err
	})
	return matches, err
}

// Shared by normal continuation and the retained notifying Retry transaction.
// The latter supplies its authorized resumed phase without releasing the fence
// or running a provider action before this lease/owner check succeeds.
func replacementActivatedOwnerMatches(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, expected domain.SessionRecord) (bool, error) {
	if !op.ActivatedSuccessorMatches(expected) {
		return false, nil
	}
	current, err := q.GetSession(ctx, expected.ID)
	if err != nil {
		return false, err
	}
	actual := rowToRecord(current)
	if actual.ControllerOwner() != expected.ControllerOwner() || actual.Metadata.BrowserCapabilityVerifier != expected.Metadata.BrowserCapabilityVerifier {
		return false, nil
	}
	doc, err := q.GetProjectDocumentState(ctx, string(op.ProjectID))
	if err != nil {
		return false, err
	}
	state, err := decodeDocumentState(doc.StateJson)
	if err != nil {
		return false, err
	}
	project, err := q.GetProject(ctx, op.ProjectID)
	if err != nil {
		return false, err
	}
	if project.ArchivedAt.Valid || state.SharedDir != op.SharedDir {
		return false, nil
	}
	sole, err := projectDocumentWriterMatches(ctx, q, op.ProjectID, expected, doc.Revision, false)
	if err != nil {
		return false, err
	}
	if !sole {
		return false, nil
	}
	l := state.Lease
	return l != nil && !l.Frozen && l.SessionID == expected.ID && l.Owner == expected.ControllerOwner() && replacementLeaseCapability(op, l) == expected.Metadata.BrowserCapabilityVerifier && l.Epoch == op.WriterActivation.AfterEpoch && l.ReplacementID == op.ID && (l.ClaimVerifier == expected.Metadata.BrowserCapabilityVerifier || l.ClaimVerifier == "" && l.TokenHash != ""), nil
}

// Legacy leases can prove only the original activation owner. Managed
// continuations record their verifier separately from the consumed claim.
func replacementLeaseCapability(op domain.OrchestratorReplacement, lease *domain.ProjectDocumentLease) string {
	if lease.CapabilityVerifier != "" {
		return lease.CapabilityVerifier
	}
	if op.WriterActivation != nil && lease.Owner == op.WriterActivation.Owner {
		return op.WriterActivation.CapabilityVerifier
	}
	return ""
}
