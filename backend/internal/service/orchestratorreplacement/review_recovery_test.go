package orchestratorreplacement_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestReplacementCancelPendingClaimRestoresManagedWrite(t *testing.T) {
	for _, afterRetry := range []bool{false, true} {
		t.Run(map[bool]string{false: "preparing", true: "after retry"}[afterRetry], func(t *testing.T) {
			x := preparingExportFixture(t)
			ctx := context.Background()
			op := x.operation
			state, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil || state.Lease == nil || state.Lease.ClaimVerifier == "" || state.Lease.TokenHash != "" {
				t.Fatal("fixture did not reach an unclaimed preparation", err)
			}
			priorEpoch := state.Lease.Epoch
			if afterRetry {
				op.Phase, op.ResumePhase = domain.ReplacementAwaitingHandoff, op.Phase
				if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
					t.Fatal(saved, err)
				}
				if _, err := x.service().Retry(ctx, op.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := x.service().Cancel(ctx, op.ID); err != nil {
				t.Fatal(err)
			}
			actor := projectdocuments.Actor{SessionID: x.source.ID, Capability: "author-secret"}
			grant, err := x.documents.Acquire(ctx, op.ProjectID, actor)
			if err != nil {
				t.Fatalf("cancel left source unable to acquire: %v", err)
			}
			if grant.Epoch <= priorEpoch {
				t.Fatal("cancelled pending claim reused its epoch")
			}
			if _, err := x.documents.Write(ctx, op.ProjectID, projectdocuments.WriteInput{Actor: actor, Lease: grant, Path: op.DocumentPath, Content: "source continues after cancellation", ExpectedHash: projectdocuments.MissingHash}); err != nil {
				t.Fatal("source could not write after cancel", err)
			}
		})
	}
}

func TestReplacementCancelReleasesUnavailableSourceWithoutReviving(t *testing.T) {
	for _, change := range []string{"terminated", "generation", "capability", "hibernated", "unobservable"} {
		t.Run(change, func(t *testing.T) {
			x := retryFrozenFixture(t)
			ctx := context.Background()
			source, _, err := x.f.store.GetSession(ctx, x.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "terminated":
				source.IsTerminated = true
				source.Activity.State = domain.ActivityExited
			case "generation":
				source.Metadata.ControllerGeneration = "replacement-review-new-generation"
			case "capability":
				source.Metadata.BrowserCapabilityVerifier = "replacement-review-new-verifier"
			case "unobservable":
				x.f.runtime.observationErr = errors.New("controller is not running")
			}
			if err := x.f.store.UpdateSession(ctx, source); err != nil {
				t.Fatal(err)
			}
			if change == "hibernated" {
				current, _, err := x.f.store.GetSession(ctx, source.ID)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				if ok, err := x.f.store.SetSessionHibernated(ctx, source.ID, current.Revision, &now); err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			before, _, err := x.f.store.GetSession(ctx, source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := x.service().Cancel(ctx, x.operation.ID); err != nil {
				t.Fatalf("pre-stop cancel retained project reservation: %v", err)
			}
			if _, active, err := x.f.store.GetActiveOrchestratorReplacement(ctx, x.operation.ProjectID); err != nil || active {
				t.Fatal("cancel retained active fence", active, err)
			}
			after, _, err := x.f.store.GetSession(ctx, source.ID)
			if err != nil || after.ControllerOwner() != before.ControllerOwner() || (after.HibernatedAt == nil) != (before.HibernatedAt == nil) {
				t.Fatal("cancel revived or changed source", err)
			}
			state, err := x.f.store.GetProjectDocumentState(ctx, source.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			if change != "unobservable" && state.Lease != nil && (state.Lease.TokenHash != "" || state.Lease.ClaimVerifier != "") {
				t.Fatal("cancel granted the stale writer a usable credential")
			}
			if *x.actualStops != 0 {
				t.Fatal("cancel attempted a provider stop")
			}
		})
	}
}

func TestReplacementRetryRechecksPreflightHistory(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.store.snapshot.NativeSessions = nil
	op, err := f.svc.Accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		op, err = f.svc.Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if op.Phase != domain.ReplacementAwaitingTranscript || op.ResumePhase != domain.ReplacementPreflight {
		t.Fatal("fixture did not reject unverified preflight", op.Phase, op.ResumePhase)
	}
	op, err = f.svc.Retry(ctx, op.ID)
	if err != nil || op.Phase != domain.ReplacementPreflight {
		t.Fatalf("retry bypassed history preflight: %s %v", op.Phase, err)
	}
	op, err = f.svc.Advance(ctx, op.ID)
	if err != nil || op.Phase != domain.ReplacementAwaitingTranscript || f.runtime.preparations != 0 || f.runtime.quiescences != 0 {
		t.Fatal("missing history reached preparation or stop", op.Phase, err)
	}
}

func expiredExportService(x *preparedExport) *replacement.Service {
	expired := x.operation.DeadlineAt.Add(time.Second)
	return replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader, Now: func() time.Time { return expired }})
}

func TestReplacementDeadlineAfterIntentStillVerifiesStop(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
	op, err := expiredExportService(x).Advance(context.Background(), x.operation.ID)
	if err != nil || op.SourceStop == nil || op.Phase != domain.ReplacementExportingTranscript || *x.actualStops != 1 {
		t.Fatalf("deadline abandoned a durable intent: phase=%s stop=%v actualStops=%d err=%v", op.Phase, op.SourceStop, *x.actualStops, err)
	}
}

func TestReplacementExpiredBusyIntentCanCancel(t *testing.T) {
	x := preparedExportFixture(t)
	ctx := context.Background()
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
	x.f.runtime.quiesce = func(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
		return domain.ReplacementControllerStop{}, domain.ErrReplacementSourceBusy
	}
	svc := expiredExportService(x)
	op, err := svc.Advance(ctx, x.operation.ID)
	if err != nil || x.f.runtime.quiescences != 1 || op.SourceStopIntentAt != nil || !op.CanReleaseSourceIntake() {
		t.Fatalf("positive no-stop Busy left an irreversible intent: phase=%s intent=%v calls=%d err=%v", op.Phase, op.SourceStopIntentAt, x.f.runtime.quiescences, err)
	}
	if _, err := svc.Cancel(ctx, op.ID); err != nil {
		t.Fatal("source positively not stopped could not cancel", err)
	}
	state, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || state.Lease.Frozen || *x.actualStops != 0 {
		t.Fatal("cancel failed to release unstopped source", err)
	}
}

func TestReplacementPostIntentConfigChangeRequiresRecovery(t *testing.T) {
	for _, boundary := range []string{"intent", "stopped", "verified"} {
		t.Run(boundary, func(t *testing.T) {
			x := preparedExportFixture(t)
			x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool {
				switch boundary {
				case "intent":
					return op.SourceStopIntentAt != nil
				case "stopped":
					return op.SourceStop != nil
				default:
					return op.Phase == domain.ReplacementTranscriptVerified
				}
			})
			ctx := context.Background()
			p, _, err := x.f.store.GetProject(ctx, string(x.source.ProjectID))
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := x.f.store.UpdateProjectSettings(ctx, string(p.ID), "renamed after stop intent", p.Config); err != nil || !ok {
				t.Fatal(ok, err)
			}
			op, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil || op.Phase != domain.ReplacementRecoveryRequired || op.ResumePhase != x.operation.Phase || op.SourceStopIntentAt == nil {
				t.Fatalf("post-intent facts regressed to pre-stop wait: %s/%s %v", op.Phase, op.ResumePhase, err)
			}
		})
	}
}

type failIntentSaveStore struct {
	*sqlite.Store
	failed bool
}

func (s *failIntentSaveStore) SaveOrchestratorReplacement(ctx context.Context, op domain.OrchestratorReplacement) (bool, error) {
	if op.SourceStopIntentAt != nil && !s.failed {
		s.failed = true
		return false, errors.New("injected stop-intent save failure")
	}
	return s.Store.SaveOrchestratorReplacement(ctx, op)
}

func TestReplacementFailedIntentSaveDoesNotPersistMutatedWait(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementExportingTranscript })
	st := &failIntentSaveStore{Store: x.f.store.Store}
	svc := replacement.New(replacement.Deps{Store: st, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader})
	_, _ = svc.Advance(context.Background(), x.operation.ID)
	op, _, err := st.GetOrchestratorReplacement(context.Background(), x.operation.ID)
	if err != nil || op.SourceStopIntentAt != nil {
		t.Fatalf("wait saved an uncommitted intent: phase=%s intent=%v err=%v", op.Phase, op.SourceStopIntentAt, err)
	}
}

func TestReplacementBusyCannotClearIntentWithUnverifiedObservation(t *testing.T) {
	for _, change := range []string{"state", "owner", "native", "provenance", "timestamp"} {
		t.Run(change, func(t *testing.T) {
			x := preparedExportFixture(t)
			x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
			x.f.runtime.quiesce = func(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
				return domain.ReplacementControllerStop{}, domain.ErrReplacementSourceBusy
			}
			switch change {
			case "state":
				x.f.runtime.observation.State = "unknown"
			case "owner":
				x.f.runtime.observation.Owner.ControllerGeneration = "other"
			case "native":
				x.f.runtime.observation.NativeID = "other"
			case "provenance":
				x.f.runtime.observation.Provenance = domain.NativeThreadChild
			case "timestamp":
				x.f.runtime.observation.ObservedAt = time.Time{}
			}
			op, err := expiredExportService(x).Advance(context.Background(), x.operation.ID)
			if err != nil || op.Phase != domain.ReplacementRecoveryRequired || op.SourceStopIntentAt == nil {
				t.Fatalf("unverified no-stop result: %s intent=%v err=%v", op.Phase, op.SourceStopIntentAt, err)
			}
			if _, err := x.service().Cancel(context.Background(), op.ID); err == nil {
				t.Fatal("unverified stop intent became cancellable")
			}
		})
	}
}

type failClearIntentStore struct {
	*sqlite.Store
	failed bool
}

func (s *failClearIntentStore) ClearReplacementStopIntent(ctx context.Context, id string, revision int64, observed ports.ReplacementSourceObservation, at time.Time) (domain.OrchestratorReplacement, bool, error) {
	if !s.failed {
		s.failed = true
		return domain.OrchestratorReplacement{}, false, errors.New("injected no-stop clearance failure")
	}
	return s.Store.ClearReplacementStopIntent(ctx, id, revision, observed, at)
}
func TestReplacementBusyClearFailureRemainsReconcilable(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
	x.f.runtime.quiesce = func(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
		return domain.ReplacementControllerStop{}, domain.ErrReplacementSourceBusy
	}
	st := &failClearIntentStore{Store: x.f.store.Store}
	svc := replacement.New(replacement.Deps{Store: st, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader, Now: func() time.Time { return x.operation.DeadlineAt.Add(time.Second) }})
	ctx := context.Background()
	if _, err := svc.Advance(ctx, x.operation.ID); err == nil {
		t.Fatal("injected write failure disappeared")
	}
	retained, _, err := st.GetOrchestratorReplacement(ctx, x.operation.ID)
	if err != nil || retained.Phase != domain.ReplacementExportingTranscript || retained.SourceStopIntentAt == nil {
		t.Fatal("clear failure stranded intent", retained.Phase, err)
	}
	forged := retained
	forged.SourceStopIntentAt = nil
	if ok, err := st.SaveOrchestratorReplacement(ctx, forged); ok || err == nil {
		t.Fatal("generic save cleared intent", ok, err)
	}
	next, err := svc.Advance(ctx, retained.ID)
	if err != nil || next.SourceStopIntentAt != nil || next.Phase != domain.ReplacementAwaitingSource {
		t.Fatal("next reconcile could not safely clear intent", next.Phase, err)
	}
}
