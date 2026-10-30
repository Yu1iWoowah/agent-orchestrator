package orchestratorreplacement_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

func TestReplacementRecoveryReserveCoversEveryRuntimeEdge(t *testing.T) {
	ctx := context.Background()
	fixtures := []struct {
		name string
		make func(*testing.T) *preparedExport
	}{
		{"retirement intent", func(t *testing.T) *preparedExport {
			x := preparedExportFixture(t)
			x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
			return x
		}},
		{"retirement receipt", func(t *testing.T) *preparedExport {
			x := preparedExportFixture(t)
			x.f.runtime.retire = func(ctx context.Context, op domain.OrchestratorReplacement, proof ports.ReplacementRetirementProof) (domain.OrchestratorReplacement, error) {
				next, ok, err := x.f.store.CommitReplacementRetirement(ctx, op.ID, op.Revision, proof.Source(), proof.DocumentRevision(), proof.Check)
				if err == nil && !ok {
					t.Fatal("retirement not applied")
				}
				return next, err
			}
			x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementRetiring })
			return x
		}},
		{"spawn intent", retiredExportFixture},
		{"controller ready", func(t *testing.T) *preparedExport { return bootstrapCoordinatorFixtureBeforeReady(t, false) }},
		{"bootstrap acceptance", bootstrapCoordinatorFixture},
		{"bootstrap settlement", func(t *testing.T) *preparedExport { return bootstrapSettlementFixture(t, false) }},
		{"resume stop intent", settledBootstrapCoordinatorFixture},
		{"resume stop receipt", func(t *testing.T) *preparedExport { return successorStopFixture(t, false) }},
		{"native resume receipt", func(t *testing.T) *preparedExport { return nativeResumeFixture(t, false) }},
		{"probe acceptance", func(t *testing.T) *preparedExport {
			x := resumedSuccessorCoordinatorFixture(t)
			x.f.runtime.probe = func(_ context.Context, op domain.OrchestratorReplacement, _ string, _ bool) (replacement.PreparationDelivery, error) {
				return replacement.PreparationDelivery{TurnID: "quota-probe", Owner: op.ResumeEvidence.AfterOwner, NativeID: op.ResumeEvidence.AfterNativeID}, nil
			}
			return x
		}},
		{"probe settlement", func(t *testing.T) *preparedExport { return probeSettlementFixture(t, false) }},
		{"writer activation", activationCoordinatorFixture},
	}
	for _, test := range fixtures {
		t.Run(test.name, func(t *testing.T) {
			x := test.make(t)
			exhaustOrdinaryRepairHistory(t, x)
			before := x.operation
			// The normal pending bootstrap fixture needs a provider acceptance, never a phase-only success.
			if test.name == "bootstrap acceptance" {
				x.f.runtime.bootstrap = func(_ context.Context, op domain.OrchestratorReplacement, _ string, _ bool) (replacement.PreparationDelivery, error) {
					return replacement.PreparationDelivery{TurnID: "quota-bootstrap", Owner: op.SuccessorController.Owner, NativeID: op.SuccessorController.Owner.ProviderConversationID}, nil
				}
			}
			next, err := x.service().Advance(ctx, before.ID)
			if err != nil || next.Revision <= before.Revision || next.Phase == domain.ReplacementRecoveryRequired || (next.Error != nil && next.Error.Code != "QUOTA_FIXTURE") {
				t.Fatalf("ordinary quota trapped %s: before=%s/%d after=%s/%d failure=%+v err=%v", test.name, before.Phase, before.Revision, next.Phase, next.Revision, next.Error, err)
			}
		})
	}
}

func TestReplacementReserveCoversSeedAndSpawnAttemptReceipts(t *testing.T) {
	x := retiredExportFixture(t)
	ctx := context.Background()
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	x.operation = op
	exhaustOrdinaryRepairHistory(t, x)
	op = x.operation
	now := time.Now().UTC()
	seed, fresh, err := x.f.store.CreateClientRequestSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, ClientRequestID: op.SpawnRequestID, ClientRequestHash: op.SpawnRequestHash, CreatedAt: now, UpdatedAt: now, Metadata: domain.SessionMetadata{Model: op.TargetAgentConfig.Model, Effort: op.TargetAgentConfig.Effort, Permissions: op.TargetAgentConfig.Permissions}})
	if err != nil || !fresh {
		t.Fatal("seed binding used ordinary budget", fresh, err)
	}
	if _, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "quota-attempt", now); err != nil {
		t.Fatal("spawn attempt used ordinary budget", err)
	}
	if ok, err := x.f.store.MarkReplacementSpawnLaunch(ctx, op.ID, seed.ID, "quota-attempt", now); err != nil || !ok {
		t.Fatal("launch receipt used ordinary budget", ok, err)
	}
	if ok, err := x.f.store.UpdateBrowserCapabilityVerifier(ctx, seed.ID, seed.ControllerOwner(), "quota-verifier"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	seed, _, err = x.f.store.GetSession(ctx, seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	seed.Metadata.ControllerGeneration, seed.Metadata.ProviderConversationID = "quota-controller", "quota-native"
	if err := x.f.store.UpdateSession(ctx, seed); err != nil {
		t.Fatal("controller receipt used ordinary budget", err)
	}
	stored, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || stored.SuccessorController == nil || len(stored.SpawnAttempts) != 1 || stored.SpawnAttempts[0].LaunchIntentAt == nil {
		t.Fatal("lost post-stop receipts", err)
	}
}

func TestReplacementReserveCoversActivationReleaseDrainAndCompletion(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	// This case exercises the no-notification completion edge. Other tests retain
	// the worker and assert it survives every runtime/provider lifecycle action.
	if ok, err := x.f.store.DeleteSession(ctx, x.worker.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	exhaustOrdinaryRepairHistory(t, x)
	activated, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil || activated.Phase != domain.ReplacementNotifying || activated.WriterActivation == nil || len(activated.Notifications) != 0 {
		t.Fatal("activation quota", activated.Phase, activated.Error, err)
	}
	done, err := x.service().Advance(ctx, activated.ID)
	for range 2 {
		if err != nil || done.Phase == domain.ReplacementCompleted {
			break
		}
		done, err = x.service().Advance(ctx, done.ID)
	}
	if err != nil || done.Phase != domain.ReplacementCompleted || done.IntakeReleasedAt == nil || done.IntakeTransfer == nil || done.IntakeDrainRequestedAt == nil {
		t.Fatalf("release/drain/completion blocked: phase=%s failure=%+v err=%v", done.Phase, done.Error, err)
	}
	for range 4 {
		next, err := x.service().Advance(ctx, done.ID)
		if err != nil || next.Revision != done.Revision {
			t.Fatal("completed poll rewrote receipt", next.Revision, err)
		}
	}
}
