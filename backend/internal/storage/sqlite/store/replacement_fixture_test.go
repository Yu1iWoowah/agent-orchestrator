package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// SeedReplacementTestFacts preloads already-produced facts for low-level CAS,
// immutability and SQL-fence tests. It is compiled only into the test binary.
// Public admission/publication tests must use real producers, never this helper:
// the generic save API deliberately cannot mint these facts anymore.
func SeedReplacementTestFacts(t testing.TB, s *Store, op domain.OrchestratorReplacement) domain.OrchestratorReplacement {
	t.Helper()
	now := op.UpdatedAt
	switch op.Phase {
	case domain.ReplacementPreparingHandoff, domain.ReplacementHandoffVerified, domain.ReplacementFreezingSharedWrites:
		if op.Preparation == nil {
			op.Preparation = &domain.ReplacementPreparation{LeaseEpoch: 7}
		}
	}
	later := op.Phase == domain.ReplacementStarting || op.Phase == domain.ReplacementNotifying || op.Phase == domain.ReplacementCompleted
	if op.FrozenAt != nil || later {
		if op.FrozenAt == nil {
			op.FrozenAt = &now
		}
		if op.Transcript == nil {
			op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}}
		}
		if len(op.Transcript.FrozenHandoff) == 0 {
			op.Transcript.FrozenHandoff = []byte("verified fixture handoff")
		}
		sum := sha256.Sum256(op.Transcript.FrozenHandoff)
		if op.Preparation == nil {
			op.Preparation = &domain.ReplacementPreparation{}
		}
		op.Preparation.SubmittedHash = hex.EncodeToString(sum[:])
		op.Preparation.LeaseEpoch = 7
		op.Preparation.VerifiedAt = &now
	}
	if later {
		op.SourceStopIntentAt = &now
		native, method := op.SourceOwner.ProviderConversationID, "chat-hibernate"
		if op.SourceOwner.Mode != domain.SessionModeChat {
			native, method = op.SourceOwner.AgentSessionID, "tui-fenced-stop"
		}
		op.SourceStop = &domain.ReplacementControllerStop{Owner: op.SourceOwner, CapabilityVerifier: op.SourceCapabilityVerifier, NativeID: native, Method: method, ConfirmedAt: now}
		op.SourceRetiredAt = &now
		tr := op.Transcript
		tr.CapturedAt, tr.ExportedAt = &now, &now
		tr.SnapshotFingerprint = strings.Repeat("a", 64)
		tr.NativeCutoffs = []domain.TranscriptEpochCutoff{{NativeID: native, SHA256: strings.Repeat("b", 64), Schema: "fixture/1", Bytes: 10, LeafID: "leaf"}}
		tr.Receipt = &domain.HandoffEvidenceReceipt{Directory: "/fixture/evidence", ManifestHash: strings.Repeat("c", 64), InputHash: strings.Repeat("d", 64)}
		tr.TranscriptHash = strings.Repeat("e", 64)
	}
	if op.Phase == domain.ReplacementNotifying || op.Phase == domain.ReplacementCompleted {
		if op.SuccessorSessionID == "" {
			op.SuccessorSessionID = "fixture-successor"
		}
		owner := op.SourceOwner
		owner.Harness = domain.HarnessClaudeCode
		owner.Mode = domain.SessionModeChat
		owner.ControllerGeneration = "successor-before"
		owner.ProviderConversationID = "native-successor"
		op.SuccessorController = &domain.ReplacementSuccessorController{Owner: owner, CapabilityVerifier: "successor-verifier", CommittedAt: now}
		op.BootstrapClientID = "fixture-bootstrap"
		op.Bootstrap = &domain.ReplacementBootstrap{PromptHash: strings.Repeat("f", 64), IntentAt: now, TurnID: "bootstrap-turn", SettledAt: &now}
		after := owner
		after.ControllerGeneration = "successor-after"
		op.ResumeProbeClientID = "fixture-probe"
		op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: owner, BeforeNativeID: owner.ProviderConversationID, BeforeCapabilityVerifier: "successor-verifier", AfterOwner: after, AfterNativeID: owner.ProviderConversationID, AfterCapabilityVerifier: "resumed-verifier", StopConfirmed: true, HistoryVerified: true, StopReceipt: &domain.ReplacementControllerStop{Owner: owner, NativeID: owner.ProviderConversationID, CapabilityVerifier: "successor-verifier", Method: "chat-hibernate", ConfirmedAt: now}, ResumeIntentAt: &now, ResumeGeneration: after.ControllerGeneration, ResumedAt: &now, VerifiedAt: &now, Probe: &domain.ReplacementContinuityProbe{Nonce: "44444444-4444-4444-8444-444444444444", PromptHash: strings.Repeat("a", 64), IntentAt: now, TurnID: "probe-turn", SettledAt: &now}}
		op.WriterActivation = &domain.ReplacementWriterActivation{BeforeEpoch: 7, AfterEpoch: 8, Owner: after, CapabilityVerifier: "resumed-verifier", ActivatedAt: now}
		if op.Phase == domain.ReplacementCompleted {
			op.IntakeReleasedAt, op.IntakeDrainRequestedAt = &now, &now
		}
	}
	if err := validateReplacement(op); err != nil {
		t.Fatal("invalid preloaded fixture", op.Phase, err)
	}
	data, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.writeDB.ExecContext(context.Background(), `UPDATE orchestrator_replacements SET phase=?,is_active=?,revision=revision+1,state_json=? WHERE id=? AND revision=?`, string(op.Phase), !op.Phase.Terminal(), string(data), op.ID, op.Revision)
	if err != nil {
		t.Fatal(err)
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		t.Fatal("fixture CAS", n, err)
	}
	op.Revision++
	return op
}
