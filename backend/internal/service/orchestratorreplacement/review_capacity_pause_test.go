package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

func TestReplacementCapacityAutomaticallyPausesAndReleasesSource(t *testing.T) {
	for _, trigger := range []string{"new stop intent", "expired deadline", "source check"} {
		t.Run(trigger, func(t *testing.T) {
			x := preparedExportFixture(t)
			ctx := context.Background()
			x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementExportingTranscript })
			const turn = "held-capacity-turn"
			if fresh, err := x.f.store.AppendUserMessage(ctx, x.conversation.ID, x.source.ID, x.source.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "held-capacity-message", ClientMessageID: "held-capacity-client", Text: "source keeps working", Origin: domain.MessageOriginHuman}, turn, time.Now()); err != nil || !fresh {
				t.Fatal(fresh, err)
			}
			if _, err := x.f.store.NextQueuedTurn(ctx, x.conversation.ID); !errors.Is(err, domain.ErrNoQueuedTurn) {
				t.Fatal("fixture did not hold the ordinary turn", err)
			}
			exhaustOrdinaryRepairHistory(t, x)
			svc := x.service()
			if trigger == "expired deadline" {
				svc = expiredExportService(x)
			}
			if trigger == "source check" {
				project, _, err := x.f.store.GetProject(ctx, string(x.operation.ProjectID))
				if err != nil {
					t.Fatal(err)
				}
				project.Config.DefaultBranch = "changed-after-freeze"
				if err := x.f.store.UpsertProject(ctx, project); err != nil {
					t.Fatal(err)
				}
			}
			op, err := svc.Advance(ctx, x.operation.ID)
			if err != nil || op.Phase != domain.ReplacementAwaitingSource || op.Error == nil || op.Error.Code != "ORCHESTRATOR_REPLACEMENT_REPAIR_CAPACITY" || op.SourceIntakeReleasedAt == nil {
				t.Fatalf("capacity trapped live source instead of persisting a pause/release: phase=%s failure=%+v released=%v err=%v", op.Phase, op.Error, op.SourceIntakeReleasedAt, err)
			}
			if op.SourceStopIntentAt != nil || op.SourceStop != nil || x.f.runtime.quiescences != 0 || *x.actualStops != 0 {
				t.Fatal("capacity pause attempted provider shutdown")
			}
			lease, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil || lease.Lease == nil || lease.Lease.Frozen {
				t.Fatal("capacity pause left writer frozen", err)
			}
			queued, err := x.f.store.NextQueuedTurn(ctx, x.conversation.ID)
			if err != nil || queued.TurnID != turn || queued.Text != "source keeps working" {
				t.Fatal("capacity pause did not release held intake", queued, err)
			}
			raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			usage := func() (int64, int64) {
				t.Helper()
				var count, bytes int64
				if err := raw.QueryRow(`SELECT count(*),sum(length(payload)) FROM orchestrator_replacement_repair_snapshots WHERE operation_id=?`, op.ID).Scan(&count, &bytes); err != nil {
					t.Fatal(err)
				}
				return count, bytes
			}
			count, bytes := usage()
			for range 5 {
				current, err := svc.Advance(ctx, op.ID)
				if err != nil || current.Revision != op.Revision || current.Phase != op.Phase || current.Error == nil || *current.Error != *op.Error {
					t.Fatal("capacity pause reconcile rewrote its receipt", current.Revision, err)
				}
			}
			if _, err := svc.Retry(ctx, op.ID); err == nil {
				t.Fatal("full ordinary budget admitted a new attempt")
			}
			noise := op
			noise.Phase = domain.ReplacementAwaitingHandoff
			noise.Error = &domain.ReplacementFailure{Code: "ORCHESTRATOR_REPLACEMENT_REPAIR_CAPACITY", Message: "waiting metadata churn"}
			if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, noise); saved || !errors.Is(err, domain.ErrReplacementRepairCapacity) {
				t.Fatal("waiting phase/metadata churn consumed reserve", saved, err)
			}
			if nextCount, nextBytes := usage(); nextCount != count || nextBytes != bytes {
				t.Fatal("reconcile/rejected retry consumed recovery capacity", count, nextCount, bytes, nextBytes)
			}
			if err := x.f.store.MarkTurnDispatching(ctx, turn); err != nil {
				t.Fatal("released turn still cannot dispatch", err)
			}
		})
	}
}

type unavailableCapacityStore struct {
	replacement.Store
	saves int
}

func (s *unavailableCapacityStore) SaveOrchestratorReplacement(context.Context, domain.OrchestratorReplacement) (bool, error) {
	s.saves++
	if s.saves > 4 {
		// Terminate an incorrect recursive implementation without hanging.
		return false, domain.ErrReplacementStateInvalid
	}
	return false, errors.Join(apierr.Conflict("WRAPPED_FAILURE", "wrapped write failure", nil), domain.ErrReplacementRepairCapacity)
}

func TestReplacementUnavailableCapacityPauseIsBounded(t *testing.T) {
	for _, trigger := range []string{"new stop intent", "expired deadline"} {
		t.Run(trigger, func(t *testing.T) {
			x := preparedExportFixture(t)
			x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementExportingTranscript })
			store := &unavailableCapacityStore{Store: x.f.store.Store}
			deps := replacement.Deps{Store: store, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader}
			if trigger == "expired deadline" {
				deps.Now = func() time.Time { return x.operation.DeadlineAt.Add(time.Second) }
			}
			svc := replacement.New(deps)
			op, err := svc.Advance(context.Background(), x.operation.ID)
			var api *apierr.Error
			if !errors.Is(err, domain.ErrReplacementRepairCapacity) || !errors.As(err, &api) || api.Code != "ORCHESTRATOR_REPLACEMENT_REPAIR_CAPACITY" || store.saves > 2 {
				t.Fatalf("capacity pause lost its bound or named cause: saves=%d api=%+v err=%v", store.saves, api, err)
			}
			if op.Phase != x.operation.Phase || op.Revision != x.operation.Revision || op.SourceStopIntentAt != nil || op.SourceIntakeReleasedAt != nil || x.f.runtime.quiescences != 0 {
				t.Fatal("failed pause exposed uncommitted recovery facts", op.Phase, op.Revision)
			}
		})
	}
}
