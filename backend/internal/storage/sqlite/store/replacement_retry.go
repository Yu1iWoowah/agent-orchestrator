package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementResetKey struct{}

func validateReplacementAttempts(op domain.OrchestratorReplacement) error {
	seen := make(map[string]bool, len(op.Attempts))
	for _, attempt := range op.Attempts {
		if !replacementUUID(attempt.ID) || seen[attempt.ID] || !attempt.Phase.Valid() || attempt.EndedAt.IsZero() || attempt.Preparation != nil && attempt.ID != attempt.Preparation.AttemptID {
			return domain.ErrReplacementStateInvalid
		}
		seen[attempt.ID] = true
		archived := op
		archived.Preparation, archived.Transcript, archived.FrozenAt = attempt.Preparation, attempt.Transcript, attempt.FrozenAt
		archived.SourceStopIntentAt, archived.SourceStop = nil, nil
		if err := validateReplacementEvidence(archived); err != nil {
			return err
		}
	}
	return nil
}

func preserveReplacementAttempts(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) (bool, error) {
	reset := ctx.Value(replacementResetKey{}) == q
	if !reset {
		if !reflect.DeepEqual(before.Attempts, after.Attempts) {
			return false, domain.ErrReplacementStateInvalid
		}
		return false, nil
	}
	if before.SourceRetiredAt != nil || before.SourceStopIntentAt != nil || before.SourceStop != nil || before.SuccessorSessionID != "" || after.Preparation != nil || after.Transcript != nil || after.FrozenAt != nil || (after.Phase != domain.ReplacementPreflight && after.Phase != domain.ReplacementCancelled) || len(after.Attempts) != len(before.Attempts)+1 {
		return false, domain.ErrReplacementStateInvalid
	}
	if !reflect.DeepEqual(before.Attempts, after.Attempts[:len(before.Attempts)]) && len(before.Attempts) != 0 {
		return false, domain.ErrReplacementStateInvalid
	}
	archived := after.Attempts[len(before.Attempts)]
	if archived.Phase != before.Phase || !reflect.DeepEqual(archived.Preparation, before.Preparation) || !reflect.DeepEqual(archived.Transcript, before.Transcript) || !replacementTimesEqual(archived.FrozenAt, before.FrozenAt) || !reflect.DeepEqual(archived.Error, before.Error) {
		return false, domain.ErrReplacementStateInvalid
	}
	if !replacementTimesEqual(archived.SourceIntakeReleasedAt, before.SourceIntakeReleasedAt) || after.SourceIntakeReleasedAt != nil {
		return false, domain.ErrReplacementStateInvalid
	}
	return true, nil
}

// ResetReplacementBeforeStop archives the current attempt and unfreezes only
// its unchanged source writer. Once shutdown was attempted, recovery must retain
// the old evidence and must not revive or allocate another source/successor.
func (s *Store) ResetReplacementBeforeStop(ctx context.Context, id string, revision int64, cancel bool, at, deadline time.Time) (domain.OrchestratorReplacement, bool, error) {
	var result domain.OrchestratorReplacement
	applied := false
	if at.IsZero() || !cancel && !deadline.After(at) {
		return result, false, domain.ErrReplacementStateInvalid
	}
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(tx, q, row)
		if err != nil {
			return err
		}
		result = op
		if len(op.Repairs) > 0 && !op.Repairs[len(op.Repairs)-1].PreStopSnapshot {
			return nil
		}
		if op.Revision != revision || op.Phase.Terminal() || op.SourceStopIntentAt != nil || op.SourceStop != nil || op.SourceRetiredAt != nil || op.SuccessorSessionID != "" {
			return nil
		}
		if !cancel && op.Phase != domain.ReplacementAwaitingSource && op.Phase != domain.ReplacementAwaitingHandoff && op.Phase != domain.ReplacementAwaitingTranscript {
			return nil
		}
		sourceRow, err := q.GetSession(tx, op.SourceSessionID)
		if err != nil && (!cancel || !errors.Is(err, sql.ErrNoRows)) {
			return err
		}
		source := rowToRecord(sourceRow)
		currentSource := source.ID == op.SourceSessionID && source.ProjectID == op.ProjectID && source.ControllerOwner() == op.SourceOwner && source.Metadata.BrowserCapabilityVerifier == op.SourceCapabilityVerifier && source.HibernatedAt == nil && source.Activity.State != domain.ActivityExited
		if !cancel {
			if !currentSource {
				return nil
			}
			matched, err := replacementProjectMatches(tx, q, op)
			if err != nil {
				return err
			}
			if !matched {
				return nil
			}
		}
		document, err := q.GetProjectDocumentState(tx, string(op.ProjectID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var state domain.ProjectDocumentState
			if err := json.Unmarshal([]byte(document.StateJson), &state); err != nil {
				return err
			}
			lease := state.Lease
			if !cancel && op.Preparation != nil && op.Preparation.LeaseEpoch > 0 && (lease == nil || lease.ReplacementID != op.ID || lease.SessionID != source.ID || lease.Owner != op.SourceOwner || lease.Epoch != op.Preparation.LeaseEpoch) {
				return nil
			}
			if !cancel && lease != nil && lease.Frozen && lease.ReplacementID != op.ID {
				return nil
			}
			if lease != nil && lease.ReplacementID == op.ID {
				if !cancel && (lease.SessionID != source.ID || lease.Owner != op.SourceOwner) {
					return nil
				}
				lease.Frozen = false
				if cancel {
					lease.ReplacementID, lease.ClaimVerifier = "", ""
					if !currentSource || lease.Owner != op.SourceOwner || op.Preparation != nil && lease.Epoch != op.Preparation.LeaseEpoch {
						// Revoke the operation's stale writer; never grant to a rotated owner.
						lease.TokenHash = ""
					}
				}
				state.Revision = document.Revision
				saved, err := s.SaveProjectDocumentState(context.WithValue(tx, projectDocumentPublicationKey{}, q), op.ProjectID, state)
				if err != nil {
					return err
				}
				if !saved {
					return domain.ErrReplacementStateInvalid
				}
			}
		} else if !cancel && (op.FrozenAt != nil || op.Preparation != nil && op.Preparation.LeaseEpoch > 0) {
			return nil
		}
		if err := settleSupersededPreparation(tx, q, op, cancel, at); err != nil {
			return err
		}
		now := at.UTC()
		op = archiveReplacementAttempt(op, uuid.NewString(), now)
		if cancel {
			op.Phase = domain.ReplacementCancelled
		} else {
			op.DeadlineAt = deadline.UTC()
		}
		saved, err := s.SaveOrchestratorReplacement(context.WithValue(tx, replacementResetKey{}, q), op)
		if err != nil {
			return err
		}
		if !saved {
			return domain.ErrReplacementStateInvalid
		}
		if cancel && currentSource {
			if err := restoreReplacementHumanProjection(tx, q, source.ID); err != nil {
				return err
			}
		}
		op.Revision++
		result, applied = op, true
		return nil
	})
	return result, applied && err == nil, err
}

// archiveReplacementAttempt preserves the current attempt for Retry/Cancel and
// for the checkpoint writer's cancellation-capacity projection. It does not
// mutate the caller's retained history or grant authority to change state.
func archiveReplacementAttempt(op domain.OrchestratorReplacement, fallbackID string, at time.Time) domain.OrchestratorReplacement {
	attemptID := fallbackID
	if op.Preparation != nil {
		attemptID = op.Preparation.AttemptID
	}
	op.Attempts = append(append([]domain.ReplacementAttempt(nil), op.Attempts...), domain.ReplacementAttempt{ID: attemptID, Phase: op.Phase, Preparation: op.Preparation, Transcript: op.Transcript, FrozenAt: op.FrozenAt, Error: op.Error, EndedAt: at, SourceIntakeReleasedAt: op.SourceIntakeReleasedAt})
	op.Preparation, op.Transcript, op.FrozenAt = nil, nil, nil
	op.SourceIntakeReleasedAt = nil
	op.Phase, op.ResumePhase, op.Error = domain.ReplacementPreflight, "", nil
	op.UpdatedAt = at
	return op
}

// Only a proven never-dispatched control turn can be cancelled here. The SQL
// archive fence also covers a receipt inserted after this transaction commits.
func settleSupersededPreparation(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, cancel bool, at time.Time) error {
	p := op.Preparation
	if op.SourceOwner.Mode != domain.SessionModeChat || p == nil || p.PromptIntentAt == nil || p.ControlClientID == "" {
		return nil
	}
	conversation, err := q.SelectConversationBySession(ctx, &op.SourceSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	row, err := q.SelectConversationMessageByClientID(ctx, gen.SelectConversationMessageByClientIDParams{ConversationID: conversation.ID, ClientMessageID: p.ControlClientID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	message := messageToDomain(row)
	sum := sha256.Sum256([]byte(message.Text))
	if message.Role != domain.MessageRoleUser || message.Origin != domain.MessageOriginAutomation || message.AuthoredByUser || message.SenderSessionID != "" || message.DeliveryContentJSON != "" || message.Continuation || message.TurnID == "" || p.ControlTurnID != "" && p.ControlTurnID != message.TurnID || hex.EncodeToString(sum[:]) != p.PromptHash {
		return domain.ErrReplacementStateInvalid
	}
	turn, err := q.SelectConversationTurnByID(ctx, message.TurnID)
	if err != nil {
		return err
	}
	if turn.ConversationID != conversation.ID || turn.HandledBySessionID != op.SourceSessionID {
		return domain.ErrReplacementStateInvalid
	}
	if turn.State.Terminal() {
		return nil
	}
	if turn.State != domain.TurnStateQueued {
		if cancel {
			return nil
		} // The current source may finish naturally; never interrupt it.
		return domain.ErrReplacementSourceBusy
	}
	if turn.ProviderTurnID != "" || turn.PromotionStartedAt.Valid {
		if cancel {
			return nil // Preserve uncertain delivery; cancellation is not a provider action.
		}
		return domain.ErrReplacementSourceBusy
	}
	count, err := q.CancelQueuedConversationTurnByID(ctx, gen.CancelQueuedConversationTurnByIDParams{CompletedAt: timeToNullTime(at.UTC()), ID: turn.ID, ConversationID: conversation.ID})
	if err != nil {
		return err
	}
	if count != 1 {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}
