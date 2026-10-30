package orchestratorreplacement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (s *Service) probeSuccessor(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	p := op.ResumeEvidence
	proof, ok := op.ExpectedSuccessorController()
	if !ok || p == nil || p.ResumedAt == nil || op.ResumeProbeClientID == "" {
		return op, domain.ErrReplacementStateInvalid
	}
	observed, err := s.deps.Runtime.ObserveReplacementSuccessor(ctx, op)
	if err != nil {
		return op, err
	}
	if observed.Owner != proof.Owner || observed.NativeID != proof.Owner.ProviderConversationID || observed.Provenance != domain.NativeThreadMain || observed.ObservedAt.IsZero() {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_RESUME_OWNER_UNVERIFIED", "Continuity probe is not on the committed resumed owner", nil)
	}
	probe, turn := p.Probe, observed.ControlTurn
	if turn != nil {
		if probe == nil || turn.ID == "" || turn.HandledBySessionID != op.SuccessorSessionID || probe.TurnID != "" && probe.TurnID != turn.ID {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_PROBE_TURN_MISMATCH", "Continuity receipt refers to another control turn", nil)
		}
		if probe.TurnID == "" {
			probe.TurnID = turn.ID
			return s.save(ctx, op)
		}
		if !turn.State.Terminal() || observed.PendingApproval {
			return op, nil
		}
		if turn.State != domain.TurnStateCompleted || turn.ProviderTurnID == "" {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_PROBE_FAILED", "Continuity probe did not complete on the native main line", nil)
		}
		if !observed.ControlAcknowledged {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_PROBE_ACK_MISSING", "Completed main probe did not acknowledge its bound nonce", nil)
		}
		if observed.State != ports.ChatControllerReady || observed.ActiveTurn != nil && !observed.ActiveTurn.State.Terminal() {
			return op, nil
		}
		now := s.deps.Now().UTC()
		probe.SettledAt, p.VerifiedAt = &now, &now
		op.Phase, op.ResumePhase, op.Error = domain.ReplacementActivatingSharedWriter, "", nil
		return s.save(ctx, op)
	}
	if probe != nil && probe.TurnID != "" {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_PROBE_RECEIPT_MISSING", "Previously accepted continuity probe cannot be found", nil)
	}
	if observed.State != ports.ChatControllerReady || observed.PendingApproval || observed.ActiveTurn != nil && !observed.ActiveTurn.State.Terminal() {
		return op, nil
	}
	allowNew := false
	if probe == nil {
		p.Probe = &domain.ReplacementContinuityProbe{Nonce: uuid.NewString(), IntentAt: s.deps.Now().UTC()}
		probe = p.Probe
		allowNew = true
	}
	prompt, err := probePrompt(op)
	if err != nil {
		return op, err
	}
	sum := sha256.Sum256([]byte(prompt))
	digest := hex.EncodeToString(sum[:])
	if allowNew {
		probe.PromptHash = digest
		op, err = s.save(ctx, op)
		if err != nil {
			return op, err
		}
	} else if probe.PromptHash != digest {
		return op, domain.ErrReplacementStateInvalid
	}
	delivery, err := s.deps.Runtime.EnsureReplacementProbe(ctx, op, prompt, allowNew)
	if err != nil {
		return op, err
	}
	if delivery.Uncertain || delivery.TurnID == "" || delivery.Owner != proof.Owner || delivery.NativeID != proof.Owner.ProviderConversationID {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_PROBE_UNCERTAIN", "Continuity acceptance cannot be confirmed; do not blindly resend", nil)
	}
	op.ResumeEvidence.Probe.TurnID = delivery.TurnID
	return s.save(ctx, op)
}

func probePrompt(op domain.OrchestratorReplacement) (string, error) {
	ack, ok := domain.ReplacementProbeAcknowledgment(op)
	if !ok {
		return "", domain.ErrReplacementStateInvalid
	}
	return fmt.Sprintf(`AO native resume continuity probe。這是 daemon 的 control turn，不是新的使用者工作。
Operation: %s
Expected native main conversation: %q
你已在同一 native conversation 完成 bootstrap，AO 已執行強 stop/resume 並驗證 required history。
Shared writer lease 仍 frozen；只回覆下列完整一行，不呼叫工具、不寫文件、不派新工作、不打斷 workers，然後結束本回合：
%s`, op.ID, op.ResumeEvidence.AfterNativeID, ack), nil
}
