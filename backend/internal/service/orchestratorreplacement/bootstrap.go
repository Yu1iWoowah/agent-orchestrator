package orchestratorreplacement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func (s *Service) bootstrap(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	if err := s.checkProject(ctx, op); err != nil {
		return op, err
	}
	if !s.deps.Now().Before(op.DeadlineAt) {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_BOOTSTRAP_TIMEOUT", "Bootstrap did not settle before the operation deadline", nil)
	}
	proof := op.SuccessorController
	if proof == nil || op.SourceRetiredAt == nil || op.BootstrapClientID == "" {
		return op, domain.ErrReplacementStateInvalid
	}
	observation, err := s.deps.Runtime.ObserveReplacementSuccessor(ctx, op)
	if err != nil {
		return op, err
	}
	if observation.Owner != proof.Owner || observation.NativeID == "" || observation.NativeID != proof.Owner.ProviderConversationID || observation.Provenance != domain.NativeThreadMain || observation.ObservedAt.IsZero() {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_UNVERIFIED", "Bootstrap controller identity is not verified", nil)
	}
	b, turn := op.Bootstrap, observation.ControlTurn
	if turn != nil {
		if b == nil || turn.ID == "" || turn.HandledBySessionID != op.SuccessorSessionID || b.TurnID != "" && b.TurnID != turn.ID {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_BOOTSTRAP_TURN_MISMATCH", "Bootstrap receipt refers to another turn", nil)
		}
		if b.TurnID == "" {
			b.TurnID = turn.ID
			return s.save(ctx, op)
		}
		if !turn.State.Terminal() || observation.PendingApproval {
			return op, nil
		}
		if turn.State != domain.TurnStateCompleted || turn.ProviderTurnID == "" {
			return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_BOOTSTRAP_FAILED", "Bootstrap did not complete on the verified native main line", nil)
		}
		if observation.State != ports.ChatControllerReady || observation.ActiveTurn != nil && !observation.ActiveTurn.State.Terminal() {
			return op, nil
		}
		now := s.deps.Now().UTC()
		b.SettledAt = &now
		op.Phase, op.Error, op.ResumePhase = domain.ReplacementHandoffSettled, nil, ""
		return s.save(ctx, op)
	}
	if b != nil && b.TurnID != "" {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_BOOTSTRAP_RECEIPT_MISSING", "Previously accepted bootstrap cannot be found", nil)
	}
	if observation.State != ports.ChatControllerReady || observation.PendingApproval || observation.ActiveTurn != nil && !observation.ActiveTurn.State.Terminal() {
		return op, nil
	}
	handoff, err := s.deps.Documents.ReadReplacementHandoff(ctx, op.ID, op.Revision)
	if err != nil {
		return op, err
	}
	prompt, err := bootstrapPrompt(op, handoff)
	if err != nil {
		return op, err
	}
	sum := sha256.Sum256([]byte(prompt))
	digest, allowNew := hex.EncodeToString(sum[:]), false
	if b == nil {
		op.Bootstrap = &domain.ReplacementBootstrap{PromptHash: digest, IntentAt: s.deps.Now().UTC()}
		op, err = s.save(ctx, op)
		if err != nil {
			return op, err
		}
		allowNew = true
	} else if b.PromptHash != digest {
		return op, domain.ErrReplacementStateInvalid
	}
	delivery, err := s.deps.Runtime.EnsureReplacementBootstrap(ctx, op, prompt, allowNew)
	if err != nil {
		return op, err
	}
	if delivery.Uncertain || delivery.TurnID == "" || delivery.Owner != proof.Owner || delivery.NativeID != proof.Owner.ProviderConversationID {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_BOOTSTRAP_UNCERTAIN", "Bootstrap acceptance cannot be confirmed; do not blindly resend", nil)
	}
	op.Bootstrap.TurnID = delivery.TurnID
	return s.save(ctx, op)
}

func bootstrapPrompt(op domain.OrchestratorReplacement, handoff projectdocuments.File) (string, error) {
	t, p := op.Transcript, op.Preparation
	if t == nil || t.Receipt == nil || p == nil || handoff.Binary || !utf8.ValidString(handoff.Content) || len(handoff.Content) > domain.MaxReplacementHandoffBytes || handoff.Path != filepath.Join(t.Receipt.Directory, "handoff.md") {
		return "", domain.ErrReplacementStateInvalid
	}
	sum := sha256.Sum256([]byte(handoff.Content))
	if hex.EncodeToString(sum[:]) != p.SubmittedHash || handoff.Hash != p.SubmittedHash {
		return "", domain.ErrReplacementStateInvalid
	}
	references, err := json.Marshal(map[string]string{"operationId": op.ID, "sourceSessionId": string(op.SourceSessionID), "successorSessionId": string(op.SuccessorSessionID), "sharedDirectory": op.SharedDir, "livingHandoffPath": op.DocumentPath, "snapshotPath": handoff.Path, "snapshotSHA256": p.SubmittedHash, "transcriptPath": filepath.Join(t.Receipt.Directory, "transcript.md"), "transcriptSHA256": t.TranscriptHash, "manifestPath": filepath.Join(t.Receipt.Directory, "manifest.json"), "manifestSHA256": t.Receipt.ManifestHash})
	if err != nil {
		return "", err
	}
	fence := "ao_handoff_snapshot_sha256_" + p.SubmittedHash
	return fmt.Sprintf(`AO orchestrator replacement bootstrap。這是新 orchestrator 的第一個 control turn。
讀取下列 immutable handoff snapshot，承接既有授權、session map、下一個 D、最後 ACK、待答 Q 與待 ACK D。不要重發 Q/D、推定答案或擴大授權，不要打斷 workers。
Shared writer lease 仍 frozen；這個回合只讀，不寫 shared living files、不派新工作、不自行 stop/replace。AO 將另行驗證同 native conversation 的 stop/resume，之後才啟用 writer。
Immutable references (JSON): %s
全文只以 path/hash 提供，未嵌入此 prompt。依需要從 transcript/manifest 查閱原文。
Immutable handoff snapshot (%d UTF-8 bytes; starts next line):
<%s>
%s
</%s>
確認 snapshot SHA256、transcript path/hash 與 §B 的未結事項，簡短回報目前停點，然後結束本回合。`, references, len(handoff.Content), fence, handoff.Content, fence), nil
}
