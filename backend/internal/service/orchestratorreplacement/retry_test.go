package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

type replacementActions interface {
	Retry(context.Context, string) (domain.OrchestratorReplacement, error)
	Cancel(context.Context, string) (domain.OrchestratorReplacement, error)
}

func TestReplacementPreStopRetryCancelArchiveAndFence(t *testing.T) {
	for _, action := range []string{"retry", "cancel"} {
		for _, change := range []string{"current", "generation", "capability", "stop intent", "lease epoch", "project config"} {
			t.Run(action+"/"+change, func(t *testing.T) {
				x := preparedExportFixture(t)
				ctx := context.Background()
				op := x.operation
				var err error
				for range 3 {
					op, err = x.service().Advance(ctx, op.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				if op.FrozenAt == nil || op.SourceStopIntentAt != nil {
					t.Fatal("fixture did not reach pre-stop frozen boundary", op.Phase)
				}
				op.Phase, op.ResumePhase = domain.ReplacementAwaitingTranscript, domain.ReplacementExportingTranscript
				op.Error = &domain.ReplacementFailure{Code: "TEST_PRE_STOP_FAILURE", Message: "retry required", Retryable: true}
				if change == "stop intent" {
					now := time.Now().UTC()
					op.SourceStopIntentAt = &now
					op.Phase = domain.ReplacementRecoveryRequired
				}
				if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
					t.Fatal(saved, err)
				}
				op, _, err = x.f.store.GetOrchestratorReplacement(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				priorPreparation := *op.Preparation
				beforeWorker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
				if err != nil {
					t.Fatal(err)
				}
				switch change {
				case "generation", "capability":
					source, _, err := x.f.store.GetSession(ctx, op.SourceSessionID)
					if err != nil {
						t.Fatal(err)
					}
					if change == "generation" {
						source.Metadata.ControllerGeneration = "rotated"
					} else {
						source.Metadata.BrowserCapabilityVerifier = "rotated"
					}
					if err := x.f.store.UpdateSession(ctx, source); err != nil {
						t.Fatal(err)
					}
				case "project config":
					project, found, err := x.f.store.GetProject(ctx, string(op.ProjectID))
					if err != nil || !found {
						t.Fatal(found, err)
					}
					project.Config.Env = map[string]string{"REPLACEMENT_TEST": "changed"}
					if saved, err := x.f.store.UpdateProjectSettings(ctx, string(op.ProjectID), project.DisplayName, project.Config); err != nil || !saved {
						t.Fatal(saved, err)
					}
				case "lease epoch":
					state, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
					if err != nil {
						t.Fatal(err)
					}
					state.Lease.Epoch++
					if saved, err := x.f.store.SaveProjectDocumentState(ctx, op.ProjectID, state); err != nil || !saved {
						t.Fatal(saved, err)
					}
				}
				beforeState, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				actions, ok := any(x.service()).(replacementActions)
				if !ok {
					t.Fatal("durable retry/cancel is unavailable")
				}
				var result domain.OrchestratorReplacement
				if action == "retry" {
					result, err = actions.Retry(ctx, op.ID)
				} else {
					result, err = actions.Cancel(ctx, op.ID)
				}
				current, _, readErr := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				state, readErr := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				afterWorker, _, readErr := x.f.store.GetSession(ctx, x.worker.ID)
				if readErr != nil || !reflect.DeepEqual(beforeWorker, afterWorker) {
					t.Fatal("action changed worker", readErr)
				}
				if action == "retry" && change == "stop intent" {
					if err != nil || current.Phase != domain.ReplacementExportingTranscript || current.SourceStopIntentAt == nil || current.SourceStop != nil || current.Revision != op.Revision+1 || len(current.Attempts) != len(op.Attempts) || !reflect.DeepEqual(beforeState, state) {
						t.Fatal("retained retry changed stop intent/lease/archive", current.Phase, err)
					}
					return
				}
				allowed := change == "current" || action == "cancel" && change != "stop intent"
				if !allowed {
					if err == nil || current.Revision != op.Revision || !reflect.DeepEqual(beforeState, state) {
						t.Fatal("unsafe action changed operation/lease", change, err)
					}
					return
				}
				if err != nil || state.Lease.Frozen || current.Preparation != nil || current.Transcript != nil || current.FrozenAt != nil {
					t.Fatal("safe pre-stop action not atomic", current.Phase, err)
				}
				encoded, err := json.Marshal(current)
				if err != nil {
					t.Fatal(err)
				}
				var history struct {
					Attempts []struct {
						Preparation *domain.ReplacementPreparation `json:"preparation"`
						Transcript  *domain.ReplacementTranscript  `json:"transcript"`
						FrozenAt    *time.Time                     `json:"frozenAt"`
					} `json:"attempts"`
				}
				if err := json.Unmarshal(encoded, &history); err != nil {
					t.Fatal(err)
				}
				if len(history.Attempts) != 1 || history.Attempts[0].Preparation == nil || !reflect.DeepEqual(*history.Attempts[0].Preparation, priorPreparation) || history.Attempts[0].Transcript == nil || history.Attempts[0].FrozenAt == nil {
					t.Fatal("retry discarded previous attempt/cutoff")
				}
				if action == "cancel" {
					if result.Phase != domain.ReplacementCancelled {
						t.Fatal(result.Phase)
					}
					if _, active, err := x.f.store.GetActiveOrchestratorReplacement(ctx, op.ProjectID); err != nil || active {
						t.Fatal("cancel retained active admission", active, err)
					}
					again, err := actions.Cancel(ctx, op.ID)
					if err != nil || again.Revision != result.Revision {
						t.Fatal("cancel replay changed receipt", err)
					}
				} else {
					if result.Phase != domain.ReplacementPreflight || result.Error != nil {
						t.Fatal(result.Phase, result.Error)
					}
					again, err := actions.Retry(ctx, op.ID)
					if err != nil || again.Revision != result.Revision {
						t.Fatal("retry replay restarted in-flight work", err)
					}
					_, err = x.service().Advance(ctx, op.ID)
					if err != nil {
						t.Fatal(err)
					}
					next, err := x.service().Advance(ctx, op.ID)
					if err != nil || next.Preparation == nil || next.Preparation.AttemptID == priorPreparation.AttemptID || next.Preparation.Nonce == priorPreparation.Nonce {
						t.Fatal("retry reused old prompt/attempt", next.Phase, err)
					}
				}
				if *x.actualStops != 0 || x.f.runtime.retirements != 0 || x.f.runtime.successors != 0 {
					t.Fatal("pre-stop action stopped/retired/spawned")
				}
			})
		}
	}
}

func retryFrozenFixture(t *testing.T) *preparedExport {
	t.Helper()
	x := preparedExportFixture(t)
	ctx := context.Background()
	op := x.operation
	var err error
	for range 3 {
		op, err = x.service().Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	op.Phase, op.ResumePhase = domain.ReplacementAwaitingTranscript, domain.ReplacementExportingTranscript
	if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	x.operation, _, err = x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestReplacementResetCASFailureRollsBackLeaseAndAttempt(t *testing.T) {
	x := retryFrozenFixture(t)
	ctx := context.Background()
	op := x.operation
	before, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER move_reset_revision AFTER UPDATE ON project_document_states BEGIN UPDATE orchestrator_replacements SET revision=revision+1 WHERE id='` + op.ID + `'; END`); err != nil {
		t.Fatal(err)
	}
	_, applied, err := x.f.store.ResetReplacementBeforeStop(ctx, op.ID, op.Revision, false, time.Now().UTC(), time.Now().UTC().Add(time.Hour))
	if err == nil || applied {
		t.Fatalf("reset CAS failure committed: applied=%t err=%v", applied, err)
	}
	after, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("CAS failure unfreezes source lease", err)
	}
	current, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || current.Revision != op.Revision || len(current.Attempts) != 0 {
		t.Fatal("CAS failure changed attempt", err)
	}
}

func TestReplacementAttemptArchiveCannotBeRewrittenThroughGenericSave(t *testing.T) {
	x := retryFrozenFixture(t)
	ctx := context.Background()
	op, err := x.service().Retry(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"drop", "edit", "append"} {
		t.Run(change, func(t *testing.T) {
			current, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "drop":
				current.Attempts = nil
			case "edit":
				current.Attempts[0].Preparation.Nonce = "changed"
			case "append":
				current.Attempts = append(current.Attempts, current.Attempts[0])
			}
			if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, current); err == nil || saved {
				t.Fatal("archive rewritten", change, saved, err)
			}
		})
	}
}

type lostResetResponse struct {
	*testStore
	calls int
}

func (s *lostResetResponse) ResetReplacementBeforeStop(ctx context.Context, id string, revision int64, cancel bool, at, deadline time.Time) (domain.OrchestratorReplacement, bool, error) {
	s.calls++
	op, applied, err := s.testStore.ResetReplacementBeforeStop(ctx, id, revision, cancel, at, deadline)
	if err == nil && applied {
		return domain.OrchestratorReplacement{}, false, errors.New("reset reply lost after commit")
	}
	return op, applied, err
}
func TestReplacementResetRereadsCommittedAttemptAfterReplyLoss(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "cancel"}[cancel], func(t *testing.T) {
			x := retryFrozenFixture(t)
			ctx := context.Background()
			store := &lostResetResponse{testStore: x.f.store}
			service := replacement.New(replacement.Deps{Store: store, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader})
			var op domain.OrchestratorReplacement
			var err error
			if cancel {
				op, err = service.Cancel(ctx, x.operation.ID)
			} else {
				op, err = service.Retry(ctx, x.operation.ID)
			}
			if err != nil || len(op.Attempts) != 1 || op.Preparation != nil || store.calls != 1 {
				t.Fatal("lost reset response discarded durable attempt", op.Phase, len(op.Attempts), err)
			}
			first := op
			if cancel {
				op, err = service.Cancel(ctx, op.ID)
			} else {
				op, err = service.Retry(ctx, op.ID)
			}
			if err != nil || op.Revision != first.Revision || store.calls != 1 || len(op.Attempts) != 1 {
				t.Fatal("replay repeated reset", err)
			}
		})
	}
}
