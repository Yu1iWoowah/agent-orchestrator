package chat

import (
	"context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (s *Service) replacementChatSuccessor(ctx context.Context, expected domain.OrchestratorReplacement) (domain.OrchestratorReplacement, domain.SessionRecord, *Controller, error) {
	reader, ok := s.sessions.(replacementHibernationStore)
	if !ok {
		return expected, domain.SessionRecord{}, nil, domain.ErrReplacementStateInvalid
	}
	op, found, err := reader.GetOrchestratorReplacement(ctx, expected.ID)
	if err != nil {
		return op, domain.SessionRecord{}, nil, err
	}
	if !found || op.Revision != expected.Revision || op.Phase != expected.Phase || op.ProjectID != expected.ProjectID || op.SourceRetiredAt == nil || op.SuccessorSessionID == "" || op.SuccessorSessionID != expected.SuccessorSessionID || op.SpawnRequestID == "" || op.SpawnRequestID != expected.SpawnRequestID || op.SpawnRequestHash == "" || op.SpawnRequestHash != expected.SpawnRequestHash {
		return op, domain.SessionRecord{}, nil, domain.ErrReplacementStateInvalid
	}
	rec, err := s.requireChatSession(ctx, op.SuccessorSessionID)
	if err != nil {
		return op, rec, nil, err
	}
	if rec.IsTerminated || rec.ProjectID != op.ProjectID || rec.Kind != domain.KindOrchestrator || rec.Harness != domain.HarnessClaudeCode || rec.ClientRequestID != op.SpawnRequestID || rec.ClientRequestHash != op.SpawnRequestHash {
		return op, rec, nil, domain.ErrReplacementStateInvalid
	}
	if rec.ProvisionState == domain.SessionProvisionProvisioning && op.Phase == domain.ReplacementStarting {
		return op, rec, nil, nil
	}
	proof, verified := op.ExpectedSuccessorController()
	if op.ActivatedSuccessorMatches(rec) {
		matches := false
		if checker, ok := s.sessions.(interface {
			ReplacementActivatedOwner(context.Context, string, domain.SessionRecord) (bool, error)
		}); ok {
			var err error
			matches, err = checker.ReplacementActivatedOwner(ctx, op.ID, rec)
			if err != nil {
				return op, rec, nil, err
			}
		} else if rec.ControllerOwner() == op.WriterActivation.Owner && rec.Metadata.BrowserCapabilityVerifier == op.WriterActivation.CapabilityVerifier {
			if fence, ok := s.sessions.(interface {
				ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error)
			}); ok {
				fenced, err := fence.ReplacementWakeFenced(ctx, rec.ID)
				if err != nil {
					return op, rec, nil, err
				}
				matches = !fenced
			}
		}
		if !matches {
			return op, rec, nil, domain.ErrReplacementIntakeFenced
		}
		proof.Owner, proof.CapabilityVerifier, verified = rec.ControllerOwner(), rec.Metadata.BrowserCapabilityVerifier, true
	}
	if rec.ProvisionState != domain.SessionProvisionReady || !verified || rec.ControllerOwner() != proof.Owner || rec.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
		return op, rec, nil, domain.ErrReplacementStateInvalid
	}
	// Read the existing controller only. Observation must never launch or resume
	// a native conversation to manufacture evidence of readiness.
	controller, err := s.Controller(rec.ID)
	if err != nil {
		return op, rec, nil, err
	}
	if controller.harness != domain.HarnessClaudeCode || controller.Generation() != proof.Owner.ControllerGeneration || controller.ProviderConversationID() != proof.Owner.ProviderConversationID {
		return op, rec, nil, domain.ErrReplacementStateInvalid
	}
	return op, rec, controller, nil
}

// ObserveReplacementChatSuccessor observes the phase's committed controller.
// Readiness alone proves neither bootstrap settlement nor same-native resume.
func (s *Service) ObserveReplacementChatSuccessor(ctx context.Context, expected domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	var observation ports.ReplacementSourceObservation
	op, rec, controller, err := s.replacementChatSuccessor(ctx, expected)
	if err != nil {
		return observation, err
	}
	if controller == nil {
		observation.State = ports.ChatControllerBusy
		return observation, nil
	}
	rows, err := s.reader.LoadConversationSnapshot(ctx, controller.ConversationID())
	if err != nil {
		return observation, err
	}
	if rows.Conversation.ActiveBranchID != controller.conversation.ActiveBranchID || rows.ActiveBranch.ID != controller.conversation.ActiveBranchID || rows.ActiveBranch.SessionID != rec.ID || rows.ActiveBranch.ProviderConversationID != controller.ProviderConversationID() {
		return observation, domain.ErrReplacementStateInvalid
	}
	observation.Owner, observation.NativeID, observation.Provenance = rec.ControllerOwner(), controller.ProviderConversationID(), domain.NativeThreadMain
	controller.mu.Lock()
	observation.State = controller.state
	if controller.pendingTurnID != "" || controller.dispatchingTurnID != "" || controller.compactionPending || controller.handoff != controllerHandoffNone {
		observation.State = ports.ChatControllerBusy
	}
	controller.mu.Unlock()
	observation.PendingApproval, err = s.store.HasPendingConversationInteractions(ctx, controller.ConversationID())
	if err != nil {
		return observation, err
	}
	for i := range rows.Turns {
		turn := &rows.Turns[i]
		if turn.HandledBySessionID == rec.ID && !turn.State.Terminal() && turn.State != domain.TurnStateQueued {
			observation.ActiveTurn = turn
			break
		}
	}
	if op.ActivatedSuccessorMatches(rec) {
		observation.ObservedAt = s.now()
		return observation, nil
	}
	control, err := s.replacementSuccessorControlTurn(ctx, op, controller)
	if err != nil {
		return observation, err
	}
	if control != nil && control.State == domain.TurnStateCompleted {
		reader, ok := s.store.(nativeThreadEvidenceReader)
		if !ok {
			return observation, &ports.NativeTranscriptError{Code: "TRANSCRIPT_PROVENANCE_UNKNOWN", Reason: "Successor control native provenance is unavailable"}
		}
		evidence, err := reader.ConversationNativeThreadEvidence(ctx, control.ConversationID, rec.ID)
		if err != nil {
			return observation, err
		}
		branch, err := s.store.ConversationBranch(ctx, control.ConversationID, control.BranchID)
		if err != nil {
			return observation, err
		}
		if branch.ProviderConversationID != observation.NativeID || domain.IndexNativeThreadEvidence(evidence).Classify(control.BranchID, control.ProviderTurnID, observation.NativeID) != domain.NativeThreadMain {
			return observation, &ports.NativeTranscriptError{Code: "TRANSCRIPT_PROVENANCE_UNKNOWN", Reason: "Successor control turn is not verified on the native main line"}
		}
		if expected, ok := domain.ReplacementProbeAcknowledgment(op); ok && op.ResumeEvidence.ResumedAt != nil {
			count := 0
			for _, message := range rows.Messages {
				if message.TurnID != control.ID || message.Role != domain.MessageRoleAssistant {
					continue
				}
				count++
				if count > domain.MaxReplacementProbeReplyMessages {
					observation.ControlAcknowledged = false
					break
				}
				if len(message.Text) <= domain.MaxReplacementProbeReplyBytes && strings.Contains(message.Text, expected) {
					observation.ControlAcknowledged = true
				}
			}
		}
	}
	observation.ControlTurn = control
	_, current, live, err := s.replacementChatSuccessor(ctx, op)
	if err != nil {
		return observation, err
	}
	if live != controller || current.ControllerOwner() != rec.ControllerOwner() || current.Metadata.BrowserCapabilityVerifier != rec.Metadata.BrowserCapabilityVerifier {
		return observation, domain.ErrReplacementStateInvalid
	}
	observation.ObservedAt = s.now().UTC()
	return observation, nil
}
