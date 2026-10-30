package chat

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// LookupReplacementChatResume reads an already published strict-replay
// controller. DB identity alone is insufficient; this never starts a provider.
func (s *Service) LookupReplacementChatResume(ctx context.Context, id domain.SessionID, generation, native string) (ports.ReplacementResumeObservation, error) {
	var zero ports.ReplacementResumeObservation
	rec, err := s.requireChatSession(ctx, id)
	if err != nil {
		return zero, err
	}
	c, err := s.Controller(id)
	if err != nil {
		return zero, err
	}
	proof := c.strictReplacementReplay
	if proof == nil {
		return zero, fmt.Errorf("%w: controller has no retained strict-replay proof", ports.ErrChatRecoveryInconclusive)
	}
	if generation == "" || native == "" || rec.IsTerminated || rec.Harness != domain.HarnessClaudeCode || rec.Metadata.ControllerGeneration != generation || rec.Metadata.ProviderConversationID != native || c.Generation() != generation || c.ProviderConversationID() != native || rec.ControllerOwner() != proof.owner || rec.Metadata.BrowserCapabilityVerifier != proof.capabilityVerifier {
		return zero, fmt.Errorf("%w: replay owner no longer matches", ports.ErrChatRecoveryInconclusive)
	}
	rows, err := s.reader.LoadConversationSnapshot(ctx, c.ConversationID())
	if err != nil {
		return zero, err
	}
	if rows.ActiveBranch.ID != c.conversation.ActiveBranchID || rows.ActiveBranch.SessionID != id || rows.ActiveBranch.ProviderConversationID != native {
		return zero, ports.ErrChatRecoveryInconclusive
	}
	c.mu.Lock()
	ready := c.state == ports.ChatControllerReady && c.handoff == controllerHandoffNone && c.pendingTurnID == "" && c.dispatchingTurnID == "" && !c.compactionPending
	c.mu.Unlock()
	if !ready {
		return zero, domain.ErrReplacementSourceBusy
	}
	return ports.ReplacementResumeObservation{Owner: rec.ControllerOwner(), NativeID: native, CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, HistoryVerified: true, ObservedAt: s.now()}, nil
}
