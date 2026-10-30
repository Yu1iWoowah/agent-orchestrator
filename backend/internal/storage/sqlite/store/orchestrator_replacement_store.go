package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementTransactionKey struct{}

func replacementUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validateReplacement(op domain.OrchestratorReplacement) error {
	_, hashErr := hex.DecodeString(op.RequestHash)
	if op.SchemaVersion != 1 || !replacementUUID(op.ID) || !replacementUUID(op.OperationKey) || len(op.RequestHash) != 64 || hashErr != nil || op.ProjectID == "" || op.SourceSessionID == "" || !op.Phase.Valid() || op.CreatedAt.IsZero() || op.UpdatedAt.IsZero() || !op.DeadlineAt.After(op.CreatedAt) {
		return domain.ErrReplacementStateInvalid
	}
	if err := validateReplacementRepairs(op); err != nil {
		return err
	}
	if err := validateReplacementStage(op); err != nil {
		return err
	}
	if op.Transcript != nil {
		if op.FrozenAt == nil || !replacementUUID(op.Transcript.AttemptID) || op.Transcript.AO.ConversationCutoffs == nil {
			return domain.ErrReplacementStateInvalid
		}
		for conversation, sequence := range op.Transcript.AO.ConversationCutoffs {
			if conversation == "" || sequence < 0 {
				return domain.ErrReplacementStateInvalid
			}
		}
	}
	if op.IntakeReleasedAt != nil && (op.IntakeReleasedAt.IsZero() || op.WriterActivation == nil || (op.Phase != domain.ReplacementNotifying && op.Phase != domain.ReplacementCompleted && op.Phase != domain.ReplacementRecoveryRequired)) {
		return domain.ErrReplacementStateInvalid
	}
	if op.IntakeDrainRequestedAt != nil && (op.IntakeDrainRequestedAt.IsZero() || op.IntakeReleasedAt == nil) {
		return domain.ErrReplacementStateInvalid
	}
	sourceReleaseAllowed := op.CanReleaseSourceIntake()
	if op.Phase == domain.ReplacementRecoveryRequired && len(op.Repairs) > 0 && op.Repairs[len(op.Repairs)-1].PreStopSnapshot {
		sourceReleaseAllowed = true
	}
	if op.SourceIntakeReleasedAt != nil && (op.SourceIntakeReleasedAt.IsZero() || !sourceReleaseAllowed) {
		return domain.ErrReplacementStateInvalid
	}
	if transfer := op.IntakeTransfer; transfer != nil {
		if op.IntakeReleasedAt == nil || !transfer.RecordedAt.Equal(*op.IntakeReleasedAt) || transfer.RetargetedMessages < 0 || transfer.MovedMessages < 0 || transfer.MovedMessages > transfer.RetargetedMessages || (transfer.RetargetedMessages == 0) != (len(transfer.SourceConversationIDs) == 0) || !slices.IsSorted(transfer.SourceConversationIDs) {
			return domain.ErrReplacementStateInvalid
		}
		for i, id := range transfer.SourceConversationIDs {
			if id == "" || i > 0 && transfer.SourceConversationIDs[i-1] == id {
				return domain.ErrReplacementStateInvalid
			}
		}
	}
	if err := validateReplacementAttempts(op); err != nil {
		return err
	}
	if err := validateReplacementSuccessorController(op); err != nil {
		return err
	}
	if err := validateReplacementBootstrap(op); err != nil {
		return err
	}
	if err := validateReplacementSpawnAttempts(op); err != nil {
		return err
	}
	if err := validateReplacementResume(op); err != nil {
		return err
	}
	if err := validateReplacementWriterActivation(op); err != nil {
		return err
	}
	return validateReplacementEvidence(op)
}

func decodeReplacement(row gen.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	var op domain.OrchestratorReplacement
	if !replacementJSONUnique([]byte(row.StateJson)) {
		return op, domain.ErrReplacementStateInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(row.StateJson))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&op); err != nil {
		return op, err
	}
	if err := validateReplacement(op); err != nil {
		return op, err
	}
	if op.ID != row.ID || string(op.ProjectID) != row.ProjectID || string(op.SourceSessionID) != row.SourceSessionID || op.OperationKey != row.OperationKey || op.RequestHash != row.RequestHash || string(op.Phase) != row.Phase || op.Phase.Terminal() == row.IsActive {
		return op, domain.ErrReplacementStateInvalid
	}
	if len(op.Repairs) > 0 && op.Repairs[len(op.Repairs)-1].AfterRevision > row.Revision {
		return op, domain.ErrReplacementStateInvalid
	}
	op.Revision = row.Revision
	op.IntakeRevision = row.IntakeRevision
	return op, nil
}

func (s *Store) replacementReader(ctx context.Context) *gen.Queries {
	for _, key := range []any{replacementTransactionKey{}, projectDocumentPublicationKey{}, conversationProjectionTxKey{}} {
		if q, ok := ctx.Value(key).(*gen.Queries); ok && q != nil {
			return q
		}
	}
	return s.qr
}

// replacementTx uses SQLite admission rather than only this Store's mutex, so
// another connection cannot accept or advance the same project concurrently.
func (s *Store) replacementTx(ctx context.Context, fn func(context.Context, *gen.Queries) error) error {
	if q := s.replacementReader(ctx); q != s.qr {
		return fn(ctx, q)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.replacementTxLocked(ctx, fn)
}

func (s *Store) replacementTxLocked(ctx context.Context, fn func(context.Context, *gen.Queries) error) error {
	conn, err := s.writeDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()
	q := gen.New(conn)
	if err := fn(context.WithValue(ctx, replacementTransactionKey{}, q), q); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// CreateOrchestratorReplacement atomically accepts one project replacement.
// A same-key replay wins before rechecking live facts that may already have moved.
func (s *Store) CreateOrchestratorReplacement(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, bool, error) {
	var result domain.OrchestratorReplacement
	created := false
	// Initial rows are requests or rejections, never pre-produced lifecycle facts.
	// In particular, Rejected must not bypass the first receipt/retirement guard.
	if op.Preparation != nil || op.FrozenAt != nil || op.Transcript != nil || op.SourceStopIntentAt != nil || op.SourceStop != nil || op.SourceRetiredAt != nil ||
		op.SuccessorSessionID != "" || op.SpawnRequestID != "" || op.SpawnRequestHash != "" || op.SpawnRetryGeneration != 0 || len(op.SpawnAttempts) != 0 ||
		op.SuccessorController != nil || op.Bootstrap != nil || op.BootstrapClientID != "" || op.ResumeProbeClientID != "" || op.ResumeEvidence != nil ||
		op.WriterActivation != nil || op.IntakeReleasedAt != nil || op.IntakeDrainRequestedAt != nil || op.SourceIntakeReleasedAt != nil || op.IntakeTransfer != nil ||
		len(op.Notifications) != 0 || len(op.Attempts) != 0 || len(op.Repairs) != 0 || op.ResumePhase != "" {
		return result, false, domain.ErrReplacementStateInvalid
	}
	if err := validateReplacement(op); err != nil {
		return result, false, err
	}
	if op.Phase != domain.ReplacementAccepted && op.Phase != domain.ReplacementRejected {
		return result, false, domain.ErrReplacementStateInvalid
	}
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		existing, err := q.GetOrchestratorReplacementByKey(tx, op.OperationKey)
		if err == nil {
			result, err = s.decodeCheckpointedReplacement(tx, q, existing)
			if err != nil {
				return err
			}
			if result.RequestHash != op.RequestHash || result.ProjectID != op.ProjectID || result.SourceSessionID != op.SourceSessionID {
				return domain.ErrReplacementKeyConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !op.Phase.Terminal() {
			active, err := q.GetActiveOrchestratorReplacement(tx, string(op.ProjectID))
			if err == nil {
				result, err = s.decodeCheckpointedReplacement(tx, q, active)
				if err != nil {
					return err
				}
				return domain.ErrReplacementInProgress
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		source, err := q.GetSession(tx, op.SourceSessionID)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrReplacementSourceChanged
		}
		if err != nil {
			return err
		}
		rec := rowToRecord(source)
		if rec.ProjectID != op.ProjectID || rec.ControllerOwner() != op.SourceOwner || rec.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
			return domain.ErrReplacementSourceChanged
		}
		if !op.Phase.Terminal() {
			matched, err := replacementProjectMatches(tx, q, op)
			if err != nil {
				return err
			}
			if !matched {
				return domain.ErrReplacementConfigChanged
			}
			if rec.IsTerminated || rec.Kind != domain.KindOrchestrator || rec.Harness != domain.HarnessClaudeCode {
				return domain.ErrReplacementSourceChanged
			}
			sessions, err := q.ListSessionsByProject(tx, optionalProjectID(op.ProjectID))
			if err != nil {
				return err
			}
			for _, other := range sessions {
				if other.ID != rec.ID && other.Kind == "orchestrator" && !other.IsTerminated {
					return domain.ErrReplacementSourceChanged
				}
			}
		}
		op.Revision = 1
		state, err := json.Marshal(op)
		if err != nil {
			return err
		}
		if err := q.InsertOrchestratorReplacement(tx, gen.InsertOrchestratorReplacementParams{
			ID: op.ID, ProjectID: string(op.ProjectID), SourceSessionID: string(op.SourceSessionID), OperationKey: op.OperationKey, RequestHash: op.RequestHash,
			Phase: string(op.Phase), IsActive: !op.Phase.Terminal(), StateJson: string(state), CreatedAt: op.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: op.UpdatedAt.UTC().Format(time.RFC3339Nano),
		}); err != nil {
			return err
		}
		if err := s.saveReplacementCheckpoint(tx, q, op.ID); err != nil {
			return err
		}
		result, created = op, true
		return nil
	})
	return result, created && err == nil, err
}

// GetOrchestratorReplacement reads an operation independently of caller lifetime.
func (s *Store) GetOrchestratorReplacement(ctx context.Context, id string) (domain.OrchestratorReplacement, bool, error) {
	return s.readReplacement(ctx, func(tx context.Context, q *gen.Queries) (gen.OrchestratorReplacement, error) {
		return q.GetOrchestratorReplacement(tx, id)
	})
}

// GetOrchestratorReplacementByKey replays the original durable request.
func (s *Store) GetOrchestratorReplacementByKey(ctx context.Context, key string) (domain.OrchestratorReplacement, bool, error) {
	return s.readReplacement(ctx, func(tx context.Context, q *gen.Queries) (gen.OrchestratorReplacement, error) {
		return q.GetOrchestratorReplacementByKey(tx, key)
	})
}

// GetActiveOrchestratorReplacement reads a project's admission reservation.
func (s *Store) GetActiveOrchestratorReplacement(ctx context.Context, id domain.ProjectID) (domain.OrchestratorReplacement, bool, error) {
	return s.readReplacement(ctx, func(tx context.Context, q *gen.Queries) (gen.OrchestratorReplacement, error) {
		return q.GetActiveOrchestratorReplacement(tx, string(id))
	})
}

// ListActiveOrchestratorReplacements discovers work after daemon restart.
func (s *Store) ListActiveOrchestratorReplacements(ctx context.Context) ([]domain.OrchestratorReplacement, error) {
	result := []domain.OrchestratorReplacement{}
	err := s.replacementRead(ctx, func(tx context.Context, q *gen.Queries) error {
		rows, err := q.ListActiveOrchestratorReplacements(tx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			op, err := s.decodeCheckpointedReplacement(tx, q, row)
			if err != nil {
				slog.ErrorContext(tx, "Skipping malformed replacement operation", "operation_id", row.ID, "project_id", row.ProjectID, "error", err)
				continue
			}
			result = append(result, op)
		}
		return nil
	})
	return result, err
}

// SaveOrchestratorReplacement changes facts only at the expected revision.
func (s *Store) SaveOrchestratorReplacement(ctx context.Context, op domain.OrchestratorReplacement) (bool, error) {
	if err := validateReplacement(op); err != nil {
		return false, err
	}
	changed := false
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, op.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		before, err := s.decodeCheckpointedReplacement(tx, q, row)
		if err != nil {
			return err
		}
		if before.Revision != op.Revision {
			return nil
		}
		if err := preserveReplacementRepairs(before, op); err != nil {
			return err
		}
		if err := validateReplacementTransition(tx, q, before, op); err != nil {
			return err
		}
		if before.ProjectID != op.ProjectID || before.SourceSessionID != op.SourceSessionID || before.OperationKey != op.OperationKey || before.RequestHash != op.RequestHash || !before.CreatedAt.Equal(op.CreatedAt) || before.SourceOwner != op.SourceOwner || before.SourceCapabilityVerifier != op.SourceCapabilityVerifier {
			return fmt.Errorf("%w: request identity is immutable", domain.ErrReplacementStateInvalid)
		}
		if before.SuccessorSessionID != "" && before.SuccessorSessionID != op.SuccessorSessionID || before.SpawnRequestID != "" && before.SpawnRequestID != op.SpawnRequestID || before.SpawnRequestHash != "" && before.SpawnRequestHash != op.SpawnRequestHash {
			return fmt.Errorf("%w: successor and spawn identity are immutable once bound", domain.ErrReplacementStateInvalid)
		}
		if before.BootstrapClientID != "" && before.BootstrapClientID != op.BootstrapClientID || before.ResumeProbeClientID != "" && before.ResumeProbeClientID != op.ResumeProbeClientID {
			return fmt.Errorf("%w: successor control identities are immutable once bound", domain.ErrReplacementStateInvalid)
		}
		if err := preserveReplacementSuccessorController(before, op); err != nil {
			return err
		}
		if err := preserveReplacementSpawnAttempts(tx, q, before, op); err != nil {
			return err
		}
		if err := preserveReplacementBootstrap(tx, q, before, op); err != nil {
			return err
		}
		if err := preserveReplacementResume(tx, q, before, op); err != nil {
			return err
		}
		if err := preserveReplacementIntakeRelease(tx, q, before, op); err != nil {
			return err
		}
		if err := preserveReplacementSourceRelease(tx, q, before, op); err != nil {
			return err
		}
		if err := preserveReplacementIntakeTransfer(tx, q, before, op); err != nil {
			return err
		}
		if err := preserveReplacementWriterActivation(tx, q, before, op); err != nil {
			return err
		}
		if before.ProjectFingerprint != op.ProjectFingerprint || before.SharedDir != op.SharedDir || before.DocumentPath != op.DocumentPath || before.TargetAgentConfig != op.TargetAgentConfig || before.TargetAccountID != op.TargetAccountID {
			return fmt.Errorf("%w: frozen project configuration is immutable", domain.ErrReplacementStateInvalid)
		}
		if before.Phase.Terminal() && op.Phase != before.Phase {
			return fmt.Errorf("%w: terminal operation cannot be reopened", domain.ErrReplacementStateInvalid)
		}
		reset, err := preserveReplacementAttempts(tx, q, before, op)
		if err != nil {
			return err
		}
		if !reset && before.Transcript != nil && (op.Transcript == nil || before.Transcript.AttemptID != op.Transcript.AttemptID || !maps.Equal(before.Transcript.AO.ConversationCutoffs, op.Transcript.AO.ConversationCutoffs)) {
			return fmt.Errorf("%w: captured AO cutoff is immutable", domain.ErrReplacementStateInvalid)
		}
		if !reset {
			if err := preserveReplacementEvidence(tx, q, before, op); err != nil {
				return err
			}
		}
		if before.SourceRetiredAt == nil && op.SourceRetiredAt != nil && tx.Value(replacementRetirementKey{}) != q {
			return domain.ErrReplacementStateInvalid
		}
		if before.SourceRetiredAt != nil && (op.SourceRetiredAt == nil || !before.SourceRetiredAt.Equal(*op.SourceRetiredAt)) {
			return fmt.Errorf("%w: retirement fact is immutable", domain.ErrReplacementStateInvalid)
		}
		if op.SourceRetiredAt != nil && op.Phase == domain.ReplacementCancelled {
			return fmt.Errorf("%w: retired source cannot be cancelled", domain.ErrReplacementStateInvalid)
		}
		if !op.Phase.Terminal() {
			active, err := q.GetActiveOrchestratorReplacement(tx, string(op.ProjectID))
			if err == nil && active.ID != op.ID {
				return domain.ErrReplacementInProgress
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		state, err := json.Marshal(op)
		if err != nil {
			return err
		}
		if _, err := q.GetLatestReplacementRepairSnapshot(tx, op.ID); errors.Is(err, sql.ErrNoRows) {
			if err := s.saveReplacementCheckpoint(tx, q, op.ID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		count, err := q.UpdateOrchestratorReplacement(tx, gen.UpdateOrchestratorReplacementParams{
			ID: op.ID, Revision: op.Revision, Phase: string(op.Phase), IsActive: !op.Phase.Terminal(), StateJson: string(state), UpdatedAt: op.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return err
		}
		changed = count == 1
		if changed {
			checkpointCtx := tx
			if replacementRecoveryProgress(tx, q, before, op) {
				checkpointCtx = replacementRecoveryCheckpointContext(tx, q)
			}
			return s.saveReplacementCheckpoint(checkpointCtx, q, op.ID)
		}
		return nil
	})
	return changed && err == nil, err
}

// GetRetiredOrchestratorReplacementBySource retains the source fence after the
// active project reservation has completed. Retired ownership cannot be revived.
func (s *Store) GetRetiredOrchestratorReplacementBySource(ctx context.Context, id domain.SessionID) (domain.OrchestratorReplacement, bool, error) {
	return s.readReplacement(ctx, func(tx context.Context, q *gen.Queries) (gen.OrchestratorReplacement, error) {
		return q.GetRetiredOrchestratorReplacementBySource(tx, string(id))
	})
}
