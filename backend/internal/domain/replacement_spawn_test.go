package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func replacementSpawnBinding() domain.OrchestratorReplacement {
	now := time.Now().UTC()
	return domain.OrchestratorReplacement{ID: "11111111-1111-4111-8111-111111111111", ProjectID: "project", SourceSessionID: "project-source", SourceRetiredAt: &now, SharedDir: "/ao/projects/project/orchestrator/shared", DocumentPath: "handoff/orchestrator-handoff.md", TargetAccountID: "target-account", TargetAgentConfig: domain.AgentConfig{Model: "sonnet", Permissions: domain.PermissionModeAuto}, Preparation: &domain.ReplacementPreparation{SubmittedHash: strings.Repeat("a", 64)}, Transcript: &domain.ReplacementTranscript{AttemptID: "22222222-2222-4222-8222-222222222222", TranscriptHash: strings.Repeat("b", 64), Receipt: &domain.HandoffEvidenceReceipt{Directory: "/ao/projects/project/orchestrator/handoffs/operation/attempt", ManifestHash: strings.Repeat("c", 64), InputHash: strings.Repeat("d", 64)}}}
}

func TestReplacementSpawnHashBindsLaunchAndEvidence(t *testing.T) {
	base := replacementSpawnBinding()
	want, err := domain.ReplacementSpawnRequestHash(base)
	if err != nil || len(want) != 64 {
		t.Fatal(want, err)
	}
	for _, change := range []string{"operation", "project", "source", "source owner", "project fingerprint", "account", "model", "permission", "launch options", "shared root", "document path", "handoff hash", "attempt", "transcript hash", "manifest hash", "input hash", "evidence root", "progress only"} {
		t.Run(change, func(t *testing.T) {
			op := replacementSpawnBinding()
			switch change {
			case "operation":
				op.ID = "different-operation"
			case "project":
				op.ProjectID = "different-project"
			case "source":
				op.SourceSessionID = "different-source"
			case "source owner":
				op.SourceOwner.ControllerGeneration = "different-generation"
			case "project fingerprint":
				op.ProjectFingerprint = strings.Repeat("e", 64)
			case "account":
				op.TargetAccountID = "different-account"
			case "model":
				op.TargetAgentConfig.Model = "opus"
			case "permission":
				op.TargetAgentConfig.Permissions = domain.PermissionModeAcceptEdits
			case "launch options":
				op.TargetAgentConfig.Launch.Ultracode = true
			case "shared root":
				op.SharedDir = "/different/shared"
			case "document path":
				op.DocumentPath = "different.md"
			case "handoff hash":
				op.Preparation.SubmittedHash = strings.Repeat("e", 64)
			case "attempt":
				op.Transcript.AttemptID = "different-attempt"
			case "transcript hash":
				op.Transcript.TranscriptHash = strings.Repeat("f", 64)
			case "manifest hash":
				op.Transcript.Receipt.ManifestHash = strings.Repeat("1", 64)
			case "input hash":
				op.Transcript.Receipt.InputHash = strings.Repeat("2", 64)
			case "evidence root":
				op.Transcript.Receipt.Directory = "/different/evidence"
			case "progress only":
				op.Revision, op.IntakeRevision, op.Phase, op.SuccessorSessionID = 99, 42, domain.ReplacementControllerReady, "project-successor"
			}
			got, err := domain.ReplacementSpawnRequestHash(op)
			if err != nil || (got == want) != (change == "progress only") {
				t.Fatalf("%s: got=%s baseline=%s err=%v", change, got, want, err)
			}
		})
	}
}

func TestReplacementSpawnHashRejectsMissingRetirementEvidence(t *testing.T) {
	for _, missing := range []string{"retired", "preparation", "transcript", "receipt", "handoff hash", "transcript hash", "account"} {
		t.Run(missing, func(t *testing.T) {
			op := replacementSpawnBinding()
			switch missing {
			case "retired":
				op.SourceRetiredAt = nil
			case "preparation":
				op.Preparation = nil
			case "transcript":
				op.Transcript = nil
			case "receipt":
				op.Transcript.Receipt = nil
			case "handoff hash":
				op.Preparation.SubmittedHash = ""
			case "transcript hash":
				op.Transcript.TranscriptHash = ""
			case "account":
				op.TargetAccountID = ""
			}
			if _, err := domain.ReplacementSpawnRequestHash(op); err == nil {
				t.Fatal("incomplete source authorized spawn binding")
			}
		})
	}
}
