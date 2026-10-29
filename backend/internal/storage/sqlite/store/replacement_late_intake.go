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

// AppendRetiredReplacementIntake accepts ordinary input addressed to the exact
// retired source. Before release it remains held; after release its insertion
// and transfer commit together. This never admits a controller or native input.
func (s *Store) AppendRetiredReplacementIntake(ctx context.Context, sourceID domain.SessionID, originalID string, message domain.ConversationMessage, turnID string, at time.Time) (domain.ConversationTurn, error) {
	var result domain.ConversationTurn
	err := s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetRetiredOrchestratorReplacementBySource(ctx, string(sourceID))
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrReplacementSourceChanged
		}
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(ctx, q, row)
		if err != nil {
			return err
		}
		sourceRow, err := q.GetSession(ctx, sourceID)
		if err != nil {
			return err
		}
		source := rowToRecord(sourceRow)
		owner := op.SourceOwner
		owner.IsTerminated = true
		if source.Kind != domain.KindOrchestrator || source.ProjectID != op.ProjectID || source.Harness != domain.HarnessClaudeCode || source.ControllerOwner() != owner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
			return domain.ErrReplacementSourceChanged
		}
		original, err := q.SelectConversationByID(ctx, originalID)
		if err != nil {
			return err
		}
		if original.ProjectID == nil || *original.ProjectID != op.ProjectID || (original.Scope != domain.ConversationScopeProject && (original.Scope != domain.ConversationScopeSession || original.SessionID == nil || *original.SessionID != sourceID)) {
			return domain.ErrReplacementSourceChanged
		}

		// Look for prior acceptance in both locations, including a turn that has
		// already drained. A changed payload never reuses its delivery identity.
		locations := []string{original.ID}
		var target domain.SessionRecord
		var destination gen.Conversation
		if op.IntakeReleasedAt != nil {
			target, destination, err = verifiedReplacementIntakeWriter(ctx, q, op)
			if err != nil {
				return err
			}
			fenced, err := q.ReplacementWakeFenced(ctx, target.ID)
			if err != nil {
				return err
			}
			if fenced {
				return domain.ErrReplacementIntakeFenced
			}
			if destination.ID != original.ID {
				locations = append(locations, destination.ID)
			}
		}
		if message.ClientMessageID != "" {
			for _, id := range locations {
				prior, err := q.SelectConversationMessageByClientID(ctx, gen.SelectConversationMessageByClientIDParams{ConversationID: id, ClientMessageID: message.ClientMessageID})
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				if !prior.ClientPayloadHash.Valid || prior.ClientPayloadHash.String != message.ClientPayloadHash {
					return domain.ErrClientMessageConflict
				}
				turn, err := q.SelectConversationTurnByID(ctx, prior.TurnID.String)
				if err != nil {
					return err
				}
				result = turnToDomain(turn)
				return nil
			}
		}
		matched, err := replacementProjectMatches(ctx, q, op)
		if err != nil {
			return err
		}
		if !matched || op.Phase.Terminal() || at.IsZero() || message.ID == "" || turnID == "" || message.ClientPayloadHash == "" {
			return domain.ErrReplacementIntakeFenced
		}
		sequence, err := q.NextConversationSequence(ctx, gen.NextConversationSequenceParams{ID: original.ID, UpdatedAt: at})
		if err != nil {
			return err
		}
		if err := q.InsertConversationTurn(ctx, gen.InsertConversationTurnParams{ID: turnID, ConversationID: original.ID, HandledBySessionID: sourceID, State: domain.TurnStateQueued, RequestedAt: at}); err != nil {
			return err
		}
		if err := q.InsertConversationMessage(ctx, gen.InsertConversationMessageParams{ID: message.ID, ConversationID: original.ID, TurnID: sql.NullString{String: turnID, Valid: true}, Sequence: sequence, Role: domain.MessageRoleUser, Origin: message.Origin, Text: message.Text, ClientMessageID: message.ClientMessageID, ClientPayloadHash: sql.NullString{String: message.ClientPayloadHash, Valid: true}, DeliveryContentJson: message.DeliveryContentJSON, SenderSessionID: message.SenderSessionID, SenderProjectID: message.SenderProjectID, SenderDisplayName: message.SenderDisplayName, CreatedAt: at, UpdatedAt: at}); err != nil {
			return err
		}
		if message.Origin == domain.MessageOriginHuman || message.AuthoredByUser {
			interactionAt := message.InteractionAt
			if interactionAt.IsZero() {
				interactionAt = at
			}
			if err := q.RecordReplacementHumanIntake(ctx, gen.RecordReplacementHumanIntakeParams{MessageID: message.ID, InteractionAt: interactionAt}); err != nil {
				return err
			}
		}
		if op.IntakeReleasedAt != nil {
			_, err := transferReplacementIntakeRows(ctx, q, op, target, destination, at, []gen.SelectReplacementPendingMessagesRow{{ID: message.ID, ConversationID: original.ID, TurnID: sql.NullString{String: turnID, Valid: true}, Sequence: sequence, ClientMessageID: message.ClientMessageID, Text: message.Text, DeliveryContentJson: message.DeliveryContentJSON, Origin: message.Origin, SenderSessionID: message.SenderSessionID, SenderProjectID: message.SenderProjectID, SenderDisplayName: message.SenderDisplayName, RequestedAt: at}})
			if err != nil {
				return err
			}
		}
		if op.IntakeReleasedAt != nil {
			if err := restoreReplacementHumanProjection(ctx, q, target.ID); err != nil {
				return err
			}
		}
		turn, err := q.SelectConversationTurnByID(ctx, turnID)
		if err != nil {
			return err
		}
		result = turnToDomain(turn)
		// Queuing is not a new native checkpoint for the retired source.
		return nil
	})
	if err != nil {
		return domain.ConversationTurn{}, err
	}
	return result, nil
}

func verifiedReplacementIntakeWriter(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement) (domain.SessionRecord, gen.Conversation, error) {
	var target domain.SessionRecord
	var conversation gen.Conversation
	fail := func() (domain.SessionRecord, gen.Conversation, error) {
		return target, conversation, domain.ErrReplacementIntakeFenced
	}
	proof, ok := op.ExpectedSuccessorController()
	if !ok || op.WriterActivation == nil {
		return fail()
	}
	row, err := q.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		return target, conversation, err
	}
	target = rowToRecord(row)
	if target.ProjectID != op.ProjectID || target.Kind != domain.KindOrchestrator || target.Mode != domain.SessionModeChat || target.Harness != domain.HarnessClaudeCode || target.ClientRequestID != op.SpawnRequestID || target.ClientRequestHash != op.SpawnRequestHash || target.HibernatedAt != nil || target.ProvisionState != domain.SessionProvisionReady || target.ControllerOwner() != proof.Owner || target.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
		return fail()
	}
	document, err := q.GetProjectDocumentState(ctx, string(op.ProjectID))
	if err != nil {
		return target, conversation, err
	}
	var state domain.ProjectDocumentState
	if err := json.Unmarshal([]byte(document.StateJson), &state); err != nil {
		return target, conversation, err
	}
	lease := state.Lease
	if lease == nil || lease.Frozen || lease.ReplacementID != op.ID || lease.Epoch != op.WriterActivation.AfterEpoch || lease.SessionID != target.ID || lease.Owner != proof.Owner || (lease.ClaimVerifier != proof.CapabilityVerifier && (lease.ClaimVerifier != "" || lease.TokenHash == "")) {
		return fail()
	}
	matched, err := projectDocumentWriterMatches(ctx, q, op.ProjectID, target, document.Revision, false)
	if err != nil {
		return target, conversation, err
	}
	if !matched {
		return fail()
	}
	conversation, err = q.SelectConversationBySession(ctx, &target.ID)
	if err != nil {
		return target, conversation, err
	}
	if conversation.Scope != domain.ConversationScopeProject || conversation.CurrentSessionID == nil || *conversation.CurrentSessionID != target.ID {
		return fail()
	}
	matched, err = replacementProjectMatches(ctx, q, op)
	if err != nil {
		return target, conversation, err
	}
	if !matched {
		return fail()
	}

	return target, conversation, nil
}
