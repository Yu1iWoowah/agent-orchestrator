package orchestratorreplacement_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

func retiredExportFixture(t *testing.T) *preparedExport {
	t.Helper()
	x := preparedExportFixture(t)
	x.f.runtime.retire = func(ctx context.Context, op domain.OrchestratorReplacement, proof ports.ReplacementRetirementProof) (domain.OrchestratorReplacement, error) {
		next, applied, err := x.f.store.CommitReplacementRetirement(ctx, op.ID, op.Revision, proof.Source(), proof.DocumentRevision(), proof.Check)
		if err != nil || !applied {
			t.Fatal(applied, err)
		}
		return next, nil
	}
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementStarting })
	return x
}

func TestReplacementSuccessorReadyRequiresPositiveBoundObservation(t *testing.T) {
	for _, change := range []string{"ready", "busy", "approval", "active turn", "unknown provenance", "wrong native", "wrong owner", "no timestamp", "missing receipt", "controller missing"} {
		t.Run(change, func(t *testing.T) {
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
			if change != "missing receipt" {
				if applied, err := x.f.store.UpdateBrowserCapabilityVerifier(ctx, seed.ID, seed.ControllerOwner(), "target-verifier"); err != nil || !applied {
					t.Fatal(applied, err)
				}
				seed.Metadata.ControllerGeneration, seed.Metadata.ProviderConversationID, seed.Metadata.BrowserCapabilityVerifier = "target-generation", "target-native", "target-verifier"
				if err := x.f.store.UpdateSession(ctx, seed); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := x.f.store.SetSessionProvisionState(ctx, seed.ID, domain.SessionProvisionReady, "", now); err != nil {
				t.Fatal(err)
			}
			seed, _, err = x.f.store.GetSession(ctx, seed.ID)
			if err != nil {
				t.Fatal(err)
			}
			x.f.runtime.successor = func(context.Context, domain.OrchestratorReplacement) (domain.SessionRecord, error) { return seed, nil }
			observation := replacement.SourceObservation{Owner: seed.ControllerOwner(), NativeID: "target-native", Provenance: domain.NativeThreadMain, State: ports.ChatControllerReady, ObservedAt: now}
			switch change {
			case "busy":
				observation.State = ports.ChatControllerBusy
			case "approval":
				observation.PendingApproval = true
			case "active turn":
				observation.ActiveTurn = &domain.ConversationTurn{State: domain.TurnStateRunning}
			case "unknown provenance":
				observation.Provenance = domain.NativeThreadUnknown
			case "wrong native":
				observation.NativeID = "different"
			case "wrong owner":
				observation.Owner.ControllerGeneration = "different"
			case "no timestamp":
				observation.ObservedAt = time.Time{}
			case "controller missing":
				x.f.runtime.successorObservationErr = errors.New("no live controller")
			}
			x.f.runtime.successorObservation = observation
			result, err := x.service().Advance(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "ready":
				if result.Phase != domain.ReplacementControllerReady || result.Error != nil {
					t.Fatalf("verified controller not advanced: %s %+v", result.Phase, result.Error)
				}
			case "busy", "approval", "active turn":
				if result.Phase != domain.ReplacementStarting {
					t.Fatalf("unsettled successor advanced: %s", result.Phase)
				}
			default:
				if result.Phase != domain.ReplacementRecoveryRequired || result.Error == nil {
					t.Fatalf("unverified successor not fenced: %s %+v", result.Phase, result.Error)
				}
			}
			if x.f.runtime.preparations != len(x.f.runtime.prompts) {
				t.Fatal("unexpected prompt effects")
			}
		})
	}
}

func TestReplacementSuccessorConfigAndDeadlineStopBeforeIntent(t *testing.T) {
	for _, which := range []string{"config changed", "deadline"} {
		t.Run(which, func(t *testing.T) {
			x := retiredExportFixture(t)
			svc := x.service()
			want := "ORCHESTRATOR_REPLACEMENT_CONFIG_CHANGED"
			if which == "config changed" {
				project, found, err := x.f.store.GetProject(context.Background(), string(x.operation.ProjectID))
				if err != nil || !found {
					t.Fatal(err)
				}
				project.Config.OrchestratorRules = "changed after retirement"
				if err := x.f.store.UpsertProject(context.Background(), project); err != nil {
					t.Fatal(err)
				}
			} else {
				want = "ORCHESTRATOR_HANDOFF_SUCCESSOR_START_TIMEOUT"
				svc = replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader, Now: func() time.Time { return x.operation.DeadlineAt.Add(time.Second) }})
			}
			result, err := svc.Advance(context.Background(), x.operation.ID)
			if err != nil || result.Phase != domain.ReplacementRecoveryRequired || result.Error == nil || result.Error.Code != want || result.SpawnRequestID != "" || result.SuccessorSessionID != "" || x.f.runtime.successors != 0 {
				t.Fatalf("%s crossed boundary: phase=%s error=%+v calls=%d err=%v", which, result.Phase, result.Error, x.f.runtime.successors, err)
			}
		})
	}
}

func TestReplacementSuccessorPartialIntentFailsClosedBeforeRuntime(t *testing.T) {
	for _, which := range []string{"key only", "hash only", "bootstrap only", "probe only", "wrong hash"} {
		t.Run(which, func(t *testing.T) {
			x := retiredExportFixture(t)
			op := x.operation
			switch which {
			case "key only":
				op.SpawnRequestID = "replacement:" + op.ID + ":successor"
			case "hash only":
				op.SpawnRequestHash = strings.Repeat("f", 64)
			case "bootstrap only":
				op.BootstrapClientID = "replacement:" + op.ID + ":bootstrap"
			case "probe only":
				op.ResumeProbeClientID = "replacement:" + op.ID + ":probe"
			case "wrong hash":
				op.SpawnRequestID, op.SpawnRequestHash, op.BootstrapClientID, op.ResumeProbeClientID = "replacement:"+op.ID+":successor", strings.Repeat("f", 64), "replacement:"+op.ID+":bootstrap", "replacement:"+op.ID+":probe"
			}
			if saved, err := x.f.store.SaveOrchestratorReplacement(context.Background(), op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			result, err := x.service().Advance(context.Background(), op.ID)
			if err != nil || result.Phase != domain.ReplacementRecoveryRequired || result.Error == nil || result.Error.Code != "ORCHESTRATOR_HANDOFF_SPAWN_INTENT_INVALID" || result.SuccessorSessionID != "" || x.f.runtime.successors != 0 {
				t.Fatalf("%s implicitly repaired/replayed: phase=%s error=%+v calls=%d err=%v", which, result.Phase, result.Error, x.f.runtime.successors, err)
			}
		})
	}
}

func TestReplacementSuccessorIntentPrecedesSeedAndRecoversOneIdentity(t *testing.T) {
	for _, loss := range []string{"none", "result lost", "coordinator lost"} {
		t.Run(loss, func(t *testing.T) {
			x := retiredExportFixture(t)
			ctx := context.Background()
			freshSeeds := 0
			x.f.runtime.successor = func(ctx context.Context, op domain.OrchestratorReplacement) (domain.SessionRecord, error) {
				stored, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
				if err != nil || stored.SpawnRequestID == "" || stored.SpawnRequestHash == "" || stored.BootstrapClientID == "" || stored.ResumeProbeClientID == "" || stored.SpawnRequestID != op.SpawnRequestID || stored.SpawnRequestHash != op.SpawnRequestHash {
					t.Fatal("spawn preceded durable intent", err)
				}
				now := time.Now().UTC()
				seed := domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, ClientRequestID: op.SpawnRequestID, ClientRequestHash: op.SpawnRequestHash, ProvisionState: domain.SessionProvisionProvisioning, CreatedAt: now, UpdatedAt: now, Metadata: domain.SessionMetadata{Model: op.TargetAgentConfig.Model, Effort: op.TargetAgentConfig.Effort, Permissions: op.TargetAgentConfig.Permissions}}
				rec, fresh, err := x.f.store.CreateClientRequestSession(ctx, seed)
				if err != nil {
					t.Fatal(err)
				}
				if fresh {
					freshSeeds++
				}
				if fresh && loss == "coordinator lost" {
					panic("lost coordinator after seed insert")
				}
				if fresh && loss == "result lost" {
					return domain.SessionRecord{}, errors.New("lost seed response")
				}
				return rec, nil
			}
			intent, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil || intent.Phase != domain.ReplacementStarting || intent.Error != nil || intent.SpawnRequestID == "" || intent.SpawnRequestHash == "" || intent.SuccessorSessionID != "" || x.f.runtime.successors != 0 {
				t.Fatalf("missing independent spawn intent: phase=%s key=%s calls=%d err=%v failure=%+v", intent.Phase, intent.SpawnRequestID, x.f.runtime.successors, err, intent.Error)
			}
			if loss == "coordinator lost" {
				assertReplacementPanicQuarantined(t, x.service(), intent.ID)
				if _, err := x.service().Retry(ctx, intent.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := x.service().Advance(ctx, intent.ID); err != nil {
					t.Fatal(err)
				}
			}
			bound, err := x.service().Get(ctx, intent.ID)
			if err != nil || bound.SuccessorSessionID == "" || bound.SourceRetiredAt == nil || bound.SpawnRequestHash != intent.SpawnRequestHash {
				t.Fatal("seed loss discarded bound identity", err)
			}
			if loss == "result lost" {
				if bound.Phase != domain.ReplacementRecoveryRequired || bound.Error == nil || bound.ResumePhase != domain.ReplacementStarting {
					t.Fatal("seed response failure not visibly recoverable", bound.Phase)
				}
			} else {
				if bound.Phase != domain.ReplacementStarting || bound.Error != nil {
					t.Fatal("unready seed advanced or failed", bound.Phase, bound.Error)
				}
				again, err := x.service().Advance(ctx, intent.ID)
				if err != nil || again.SuccessorSessionID != bound.SuccessorSessionID || again.Phase != domain.ReplacementStarting || again.Error != nil {
					t.Fatal("poll/restart changed unready successor", err)
				}
			}
			if freshSeeds != 1 || *x.actualStops != 1 || x.f.runtime.retirements != 1 {
				t.Fatal("restart created another seed or repeated retirement")
			}
		})
	}
}

func TestReplacementSuccessorControlIDsAreImmutableAfterIntent(t *testing.T) {
	for _, which := range []string{"bootstrap", "resume probe"} {
		t.Run(which, func(t *testing.T) {
			x := retiredExportFixture(t)
			op, err := x.service().Advance(context.Background(), x.operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if which == "bootstrap" {
				op.BootstrapClientID = "replacement:changed:bootstrap"
			} else {
				op.ResumeProbeClientID = "replacement:changed:probe"
			}
			if saved, err := x.f.store.SaveOrchestratorReplacement(context.Background(), op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
				t.Fatalf("control identity changed after intent: saved=%t err=%v", saved, err)
			}
		})
	}
}
