package orchestratorreplacement_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

func bootstrapCoordinatorFixture(t *testing.T) *preparedExport {
	return bootstrapCoordinatorFixtureBeforeReady(t, true)
}

func bootstrapCoordinatorFixtureBeforeReady(t *testing.T, ready bool) *preparedExport {
	t.Helper()
	x := retiredExportFixture(t)
	ctx := context.Background()
	intent, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed, _, err := x.f.store.CreateClientRequestSession(ctx, domain.SessionRecord{ProjectID: intent.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, ClientRequestID: intent.SpawnRequestID, ClientRequestHash: intent.SpawnRequestHash, ProvisionState: domain.SessionProvisionProvisioning, Metadata: domain.SessionMetadata{Model: intent.TargetAgentConfig.Model, Effort: intent.TargetAgentConfig.Effort, Permissions: intent.TargetAgentConfig.Permissions}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := x.f.store.UpdateBrowserCapabilityVerifier(ctx, seed.ID, seed.ControllerOwner(), "target-verifier"); err != nil || !applied {
		t.Fatal(applied, err)
	}
	seed.Metadata.ControllerGeneration, seed.Metadata.ProviderConversationID, seed.Metadata.BrowserCapabilityVerifier = "target-generation", "target-native", "target-verifier"
	if err := x.f.store.UpdateSession(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.store.SetSessionProvisionState(ctx, seed.ID, domain.SessionProvisionReady, "", now); err != nil {
		t.Fatal(err)
	}
	seed, _, err = x.f.store.GetSession(ctx, seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	x.f.runtime.successor = func(context.Context, domain.OrchestratorReplacement) (domain.SessionRecord, error) { return seed, nil }
	x.f.runtime.successorObservation = replacement.SourceObservation{Owner: seed.ControllerOwner(), NativeID: seed.Metadata.ProviderConversationID, Provenance: domain.NativeThreadMain, State: ports.ChatControllerReady, ObservedAt: now}
	x.operation, _, err = x.f.store.GetOrchestratorReplacement(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		return x
	}
	x.operation, err = x.service().Advance(ctx, intent.ID)
	if err != nil || x.operation.Phase != domain.ReplacementControllerReady {
		t.Fatal(x.operation.Phase, err)
	}
	return x
}

func TestReplacementBootstrapIntentAcceptanceAndSettlementAreSeparate(t *testing.T) {
	for _, result := range []string{"completed", "running", "failed", "approval", "wrong turn", "uncertain", "crash before send", "response lost"} {
		t.Run(result, func(t *testing.T) {
			x := bootstrapCoordinatorFixture(t)
			ctx := context.Background()
			sends := 0
			x.f.runtime.bootstrap = func(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (replacement.PreparationDelivery, error) {
				stored, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
				sum := sha256.Sum256([]byte(prompt))
				if err != nil || stored.Bootstrap == nil || stored.Bootstrap.IntentAt.IsZero() || stored.Bootstrap.PromptHash != hex.EncodeToString(sum[:]) {
					t.Fatal("send preceded exact durable intent", err)
				}
				snapshot, err := os.ReadFile(filepath.Join(op.Transcript.Receipt.Directory, "handoff.md"))
				if err != nil || !strings.Contains(prompt, string(snapshot)) || !strings.Contains(prompt, filepath.Join(op.Transcript.Receipt.Directory, "transcript.md")) || !strings.Contains(prompt, op.Transcript.TranscriptHash) || !strings.Contains(prompt, op.SharedDir) {
					t.Fatal("bootstrap omitted snapshot/evidence references", err)
				}
				full, err := os.ReadFile(filepath.Join(op.Transcript.Receipt.Directory, "transcript.md"))
				if err != nil || len(full) == 0 || strings.Contains(prompt, string(full)) {
					t.Fatal("bootstrap embedded full transcript", err)
				}
				if !allowNew || result == "uncertain" {
					return replacement.PreparationDelivery{Uncertain: true}, nil
				}
				if result == "crash before send" {
					panic("lost bootstrap caller before send")
				}
				sends++
				if result == "response lost" {
					return replacement.PreparationDelivery{}, errors.New("bootstrap response lost")
				}
				return replacement.PreparationDelivery{TurnID: "bootstrap-turn", Owner: op.SuccessorController.Owner, NativeID: op.SuccessorController.Owner.ProviderConversationID}, nil
			}
			if result == "crash before send" {
				assertReplacementPanicQuarantined(t, x.service(), x.operation.ID)
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if result == "uncertain" || result == "crash before send" || result == "response lost" {
				if op.Phase != domain.ReplacementRecoveryRequired || op.Bootstrap == nil || op.Bootstrap.TurnID != "" || sends > 1 {
					t.Fatal("uncertainty replayed or discarded intent", op.Phase, sends)
				}
				return
			}
			if op.Phase != domain.ReplacementControllerReady || op.Bootstrap == nil || op.Bootstrap.TurnID != "bootstrap-turn" || op.Bootstrap.SettledAt != nil || sends != 1 {
				t.Fatal("acceptance claimed completion", op.Phase, sends)
			}
			turn := &domain.ConversationTurn{ID: op.Bootstrap.TurnID, HandledBySessionID: op.SuccessorSessionID, State: domain.TurnStateCompleted, ProviderTurnID: "native-bootstrap"}
			switch result {
			case "running":
				turn.State = domain.TurnStateRunning
			case "failed":
				turn.State = domain.TurnStateFailed
			case "approval":
				x.f.runtime.successorObservation.PendingApproval = true
			case "wrong turn":
				turn.ID = "another-turn"
			}
			x.f.runtime.successorObservation.ControlTurn = turn
			op, err = x.service().Advance(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch result {
			case "completed":
				if op.Phase != domain.ReplacementHandoffSettled || op.Bootstrap.SettledAt == nil {
					t.Fatal("main bootstrap not settled", op.Phase, op.Error)
				}
			case "running", "approval":
				if op.Phase != domain.ReplacementControllerReady || op.Bootstrap.SettledAt != nil {
					t.Fatal("pending bootstrap advanced", op.Phase)
				}
			default:
				if op.Phase != domain.ReplacementRecoveryRequired || op.Error == nil {
					t.Fatal("failed or mismatched bootstrap advanced", op.Phase)
				}
			}
			if sends != 1 {
				t.Fatal("bootstrap resent", sends)
			}
		})
	}
}

func TestReplacementBootstrapWaitsBeforeCreatingIntent(t *testing.T) {
	for _, unsettled := range []string{"busy", "approval", "active turn"} {
		t.Run(unsettled, func(t *testing.T) {
			x := bootstrapCoordinatorFixture(t)
			switch unsettled {
			case "busy":
				x.f.runtime.successorObservation.State = ports.ChatControllerBusy
			case "approval":
				x.f.runtime.successorObservation.PendingApproval = true
			case "active turn":
				x.f.runtime.successorObservation.ActiveTurn = &domain.ConversationTurn{State: domain.TurnStateRunning}
			}
			op, err := x.service().Advance(context.Background(), x.operation.ID)
			if err != nil || op.Phase != domain.ReplacementControllerReady || op.Bootstrap != nil || x.f.runtime.bootstraps != 0 {
				t.Fatal("unsettled target received bootstrap", op.Phase, err)
			}
		})
	}
}
