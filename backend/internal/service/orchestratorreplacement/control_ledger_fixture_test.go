package orchestratorreplacement_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

// The fake provider still uses the real canonical ledger. Only rows created by
// this helper are projected on observation; preexisting/tampered ledger tests
// remain independent and cannot be silently repaired by the fake.
func (r *runtime) acceptControl(ctx context.Context, op domain.OrchestratorReplacement, prompt, clientID string, d replacement.PreparationDelivery) error {
	if d.Uncertain || d.TurnID == "" {
		return nil
	}
	target, _, err := r.controlStore.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		return err
	}
	c, err := r.controlStore.CreateProjectConversationAdoptingQueue(ctx, "control-project", op.ProjectID, target.ID, domain.ConversationActivity{}, time.Now())
	if err != nil {
		return err
	}
	if _, found, err := r.controlStore.ConversationMessageByClientID(ctx, c.ID, clientID); err != nil || found {
		return err
	}
	b, err := r.controlStore.ConversationBranch(ctx, c.ID, c.ActiveBranchID)
	if err != nil || b.ProviderConversationID != target.Metadata.ProviderConversationID || b.SessionID != target.ID {
		branch := domain.ConversationBranch{ID: "control-root-" + target.Metadata.ControllerGeneration, ConversationID: c.ID, SessionID: target.ID, ParentBranchID: c.ActiveBranchID, ProviderScopeID: "control-root-" + target.Metadata.ControllerGeneration, ProviderConversationID: target.Metadata.ProviderConversationID, Strategy: domain.ConversationBranchStrategyNative, ForkAfterSequence: c.LatestSequence, CreatedAt: time.Now()}
		if err := r.controlStore.CommitChatSpawn(ctx, target, branch); err != nil {
			return err
		}
	}
	_, err = r.controlStore.AppendUserMessage(ctx, c.ID, target.ID, target.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "control-message-" + d.TurnID, ClientMessageID: clientID, Text: prompt, Origin: domain.MessageOriginAutomation}, d.TurnID, time.Now())
	if err == nil {
		if r.controls == nil {
			r.controls = map[string]string{}
		}
		r.controls[d.TurnID] = c.ID
	}
	return err
}

func (r *runtime) projectControl(ctx context.Context, op domain.OrchestratorReplacement) error {
	t := r.successorObservation.ControlTurn
	if t == nil || r.controls[t.ID] == "" || !t.State.Terminal() {
		return nil
	}
	conv := r.controls[t.ID]
	actual, err := r.controlStore.TurnByID(ctx, t.ID)
	if err != nil {
		return err
	}
	if actual.State.Terminal() {
		return nil
	}
	if t.ProviderTurnID != "" {
		if err := r.controlStore.BindTurnToProvider(ctx, t.ID, t.ProviderTurnID, time.Now()); err != nil {
			return err
		}
	}
	if t.State == domain.TurnStateCompleted {
		native := r.successorObservation.NativeID
		if r.successorObservation.Provenance == domain.NativeThreadChild {
			native = "child-of-" + native
		}
		payload, err := json.Marshal(map[string]string{"providerTurnId": t.ProviderTurnID, "providerConversationId": native, "turnState": "completed"})
		if err != nil {
			return err
		}
		target, _, err := r.controlStore.GetSession(ctx, op.SuccessorSessionID)
		if err != nil {
			return err
		}
		if _, err := r.controlStore.ProjectProviderEvent(ctx, conv, target.ID, target.Metadata.ControllerGeneration, "control-proof-"+t.ID, "turn.completed", string(payload), time.Now(), func(context.Context) error { return nil }); err != nil {
			return err
		}
		if op.ResumeEvidence != nil && op.ResumeEvidence.Probe != nil && r.successorObservation.ControlAcknowledged {
			ack, ok := domain.ReplacementProbeAcknowledgment(op)
			if !ok {
				return fmt.Errorf("fixture nonce missing")
			}
			if err := r.controlStore.SettleAssistantMessage(ctx, conv, "control-reply-"+t.ID, t.ProviderTurnID, ack, "control-answer-"+t.ID, time.Now()); err != nil {
				return err
			}
		}
	}
	return r.controlStore.SettleTurnByID(ctx, t.ID, t.State, "fixture terminal", time.Now())
}
