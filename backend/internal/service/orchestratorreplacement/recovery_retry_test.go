package orchestratorreplacement_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

func TestReplacementRecoveryRetryRetainsDurableSideEffects(t *testing.T) {
	for _, stage := range []string{"source stopped", "source retired", "bootstrap accepted", "native resumed", "writer activated"} {
		t.Run(stage, func(t *testing.T) {
			var x *preparedExport
			ctx := context.Background()
			switch stage {
			case "source stopped":
				x = preparedExportFixture(t)
				x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
			case "source retired":
				x = retiredExportFixture(t)
			case "bootstrap accepted":
				x = bootstrapCoordinatorFixture(t)
				x.f.runtime.bootstrap = func(_ context.Context, op domain.OrchestratorReplacement, _ string, _ bool) (replacement.PreparationDelivery, error) {
					return replacement.PreparationDelivery{Owner: op.SuccessorController.Owner, NativeID: op.SuccessorController.Owner.ProviderConversationID, TurnID: "retained-bootstrap-turn"}, nil
				}
				var err error
				x.operation, err = x.service().Advance(ctx, x.operation.ID)
				if err != nil || x.operation.Bootstrap == nil || x.operation.Bootstrap.TurnID == "" {
					t.Fatal("fixture bootstrap acceptance missing", err)
				}
			case "native resumed":
				x = resumedSuccessorCoordinatorFixture(t)
			case "writer activated":
				x = activationCoordinatorFixture(t)
				var err error
				x.operation, err = x.documents.ActivateReplacementWriter(ctx, x.operation.ID, x.operation.Revision)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := x.operation
			op := before
			op.Phase, op.ResumePhase = domain.ReplacementRecoveryRequired, before.Phase
			op.Error = &domain.ReplacementFailure{Code: "TEST_TRANSIENT_READ_FAILURE", Message: "verify retained boundary", Retryable: true}
			if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			sourceBefore, _, err := x.f.store.GetSession(ctx, op.SourceSessionID)
			if err != nil {
				t.Fatal(err)
			}
			workerBefore, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			stateBefore, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			stops, retires, spawns, bootstraps, resumes, probes := x.f.runtime.quiescences, x.f.runtime.retirements, x.f.runtime.successors, x.f.runtime.bootstraps, x.f.runtime.nativeResumes, x.f.runtime.probes
			result, err := x.service().Retry(ctx, op.ID)
			if err != nil || result.Phase != before.Phase || result.Error != nil || result.ResumePhase != "" {
				t.Fatal("retained boundary was not reconciled", result.Phase, result.ResumePhase, err)
			}
			if !reflect.DeepEqual(result.Preparation, before.Preparation) || !reflect.DeepEqual(result.Transcript, before.Transcript) || !reflect.DeepEqual(result.SourceStop, before.SourceStop) || !reflect.DeepEqual(result.Bootstrap, before.Bootstrap) || !reflect.DeepEqual(result.ResumeEvidence, before.ResumeEvidence) || !reflect.DeepEqual(result.WriterActivation, before.WriterActivation) || result.SuccessorSessionID != before.SuccessorSessionID || result.SpawnRequestID != before.SpawnRequestID || len(result.Attempts) != len(before.Attempts) {
				t.Fatal("recovery retry rewrote original evidence or identity")
			}
			if x.f.runtime.quiescences != stops || x.f.runtime.retirements != retires || x.f.runtime.successors != spawns || x.f.runtime.bootstraps != bootstraps || x.f.runtime.nativeResumes != resumes || x.f.runtime.probes != probes {
				t.Fatal("retry repeated provider/runtime side effect")
			}
			sourceAfter, _, err := x.f.store.GetSession(ctx, op.SourceSessionID)
			if err != nil || !reflect.DeepEqual(sourceBefore, sourceAfter) {
				t.Fatal("retry revived or changed source", err)
			}
			workerAfter, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil || !reflect.DeepEqual(workerBefore, workerAfter) {
				t.Fatal("retry changed worker", err)
			}
			stateAfter, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil || !reflect.DeepEqual(stateBefore, stateAfter) {
				t.Fatal("retry changed lease", err)
			}
			again, err := x.service().Retry(ctx, op.ID)
			if err != nil || again.Revision != result.Revision {
				t.Fatal("retry of in-flight boundary changed operation", err)
			}
			if _, err := x.service().Cancel(ctx, op.ID); err == nil {
				t.Fatal("stop/retired operation was cancellable")
			}
			if stage == "bootstrap accepted" {
				next, err := x.service().Advance(ctx, op.ID)
				if err != nil || x.f.runtime.bootstraps != bootstraps || !reflect.DeepEqual(next.Bootstrap, before.Bootstrap) {
					t.Fatal("recovery blindly resent previously accepted bootstrap", err)
				}
			}
		})
	}
}

func TestReplacementRecoveryRetryRejectsUnboundResumePhase(t *testing.T) {
	x := retiredExportFixture(t)
	ctx := context.Background()
	op := x.operation
	op.Phase, op.ResumePhase = domain.ReplacementRecoveryRequired, domain.ReplacementPreparingHandoff
	if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	current, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.service().Retry(ctx, op.ID); err == nil {
		t.Fatal("retired source rewind accepted")
	}
	after, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || after.Revision != current.Revision || after.SourceRetiredAt == nil {
		t.Fatal("refused recovery changed state", err)
	}
}

func TestReplacementRecoveryRetryRefusesChangedOwnershipAndConfig(t *testing.T) {
	for _, change := range []string{"source capability", "successor generation", "successor capability", "project config"} {
		t.Run(change, func(t *testing.T) {
			x := resumedSuccessorCoordinatorFixture(t)
			ctx := context.Background()
			op := x.operation
			op.Phase, op.ResumePhase = domain.ReplacementRecoveryRequired, domain.ReplacementVerifyingResume
			if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			op, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if change == "project config" {
				project, found, err := x.f.store.GetProject(ctx, string(op.ProjectID))
				if err != nil || !found {
					t.Fatal(found, err)
				}
				project.Config.Env = map[string]string{"REPLACEMENT_TEST": "changed"}
				if saved, err := x.f.store.UpdateProjectSettings(ctx, string(op.ProjectID), project.DisplayName, project.Config); err != nil || !saved {
					t.Fatal(saved, err)
				}
			} else {
				id := op.SuccessorSessionID
				if change == "source capability" {
					id = op.SourceSessionID
				}
				rec, _, err := x.f.store.GetSession(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if change == "successor generation" {
					rec.Metadata.ControllerGeneration = "rotated"
				} else {
					rec.Metadata.BrowserCapabilityVerifier = "rotated"
				}
				if err := x.f.store.UpdateSession(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := x.service().Retry(ctx, op.ID); err == nil {
				t.Fatal("changed authority resumed recovery", change)
			}
			after, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil || after.Revision != op.Revision || after.Phase != domain.ReplacementRecoveryRequired || after.SuccessorSessionID != op.SuccessorSessionID {
				t.Fatal("refused recovery changed operation", err)
			}
		})
	}
}
