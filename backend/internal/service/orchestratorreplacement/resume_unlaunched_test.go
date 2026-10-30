package orchestratorreplacement_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReplacementKnownUnlaunchedResumeCanRetryWithoutLosingHistory(t *testing.T) {
	x := stoppedSuccessorCoordinatorFixture(t)
	ctx := context.Background()
	stop := *x.operation.ResumeEvidence.StopReceipt
	var rejectedGeneration string
	x.f.runtime.nativeResume = func(_ context.Context, op domain.OrchestratorReplacement, allowNew bool) (ports.ReplacementResumeObservation, error) {
		if !allowNew || op.ResumeEvidence.ResumeIntentAt == nil {
			t.Fatal("fixture did not reach a fresh persisted resume intent")
		}
		rejectedGeneration = op.ResumeEvidence.ResumeGeneration
		return ports.ReplacementResumeObservation{}, fmt.Errorf("%w: account unavailable before launch", ports.ErrChatResumeNotLaunched)
	}
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil || op.Phase != domain.ReplacementRecoveryRequired || op.Error == nil || op.Error.Code != "ORCHESTRATOR_HANDOFF_RESUME_NOT_LAUNCHED" || op.ResumeEvidence.ResumeIntentAt != nil || op.ResumeEvidence.ResumeGeneration != "" || len(op.ResumeEvidence.UnlaunchedAttempts) != 1 {
		t.Fatalf("known no-launch cannot recover: phase=%s failure=%+v resume=%+v err=%v", op.Phase, op.Error, op.ResumeEvidence, err)
	}
	if receipt := op.ResumeEvidence.UnlaunchedAttempts[0]; receipt.Generation != rejectedGeneration || receipt.IntentAt.IsZero() || receipt.ConfirmedAt.IsZero() || !reflect.DeepEqual(*op.ResumeEvidence.StopReceipt, stop) {
		t.Fatal("unlaunched attempt or strong stop history lost", receipt)
	}
	if replay, saved, err := x.f.store.RecordReplacementResumeUnlaunched(ctx, op.ID, op.Revision-1, rejectedGeneration, "reply lost", time.Now().UTC()); err != nil || !saved || replay.Revision != op.Revision {
		t.Fatal("lost no-launch reply consumed another checkpoint", saved, err)
	}
	for _, runtimeSave := range []bool{false, true} {
		forged := op
		p := *op.ResumeEvidence
		p.UnlaunchedAttempts = append(append([]domain.ReplacementResumeUnlaunched{}, p.UnlaunchedAttempts...), domain.ReplacementResumeUnlaunched{Generation: "forged-generation", IntentAt: time.Now(), ConfirmedAt: time.Now()})
		forged.ResumeEvidence = &p
		var saved bool
		if runtimeSave {
			saved, err = x.f.store.SaveReplacementRuntimeProgress(ctx, forged)
		} else {
			saved, err = x.f.store.SaveOrchestratorReplacement(ctx, forged)
		}
		if saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
			t.Fatal("generic publication minted a no-launch receipt", runtimeSave, saved, err)
		}
	}
	if _, err := x.service().Retry(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	launched := 0
	x.f.runtime.nativeResume = func(ctx context.Context, op domain.OrchestratorReplacement, allowNew bool) (ports.ReplacementResumeObservation, error) {
		p := op.ResumeEvidence
		if !allowNew || p.ResumeGeneration == rejectedGeneration {
			t.Fatal("retry reused a failed permission or was still lookup-only")
		}
		launched++
		rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
		if err != nil {
			return ports.ReplacementResumeObservation{}, err
		}
		rec.Metadata.ControllerGeneration, rec.Metadata.BrowserCapabilityVerifier = p.ResumeGeneration, "verified-new-capability"
		if err := x.f.store.UpdateSession(ctx, rec); err != nil {
			return ports.ReplacementResumeObservation{}, err
		}
		rec, _, _ = x.f.store.GetSession(ctx, rec.ID)
		if applied, err := x.f.store.SetSessionHibernated(ctx, rec.ID, rec.Revision, nil); err != nil || !applied {
			t.Fatal(applied, err)
		}
		rec, _, _ = x.f.store.GetSession(ctx, rec.ID)
		return ports.ReplacementResumeObservation{Owner: rec.ControllerOwner(), NativeID: rec.Metadata.ProviderConversationID, CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, HistoryVerified: true, ObservedAt: time.Now().UTC()}, nil
	}
	op, err = x.service().Advance(ctx, op.ID)
	if err != nil || op.ResumeEvidence.ResumedAt == nil || len(op.ResumeEvidence.UnlaunchedAttempts) != 1 || launched != 1 || op.ResumeEvidence.AfterNativeID != stop.NativeID {
		t.Fatal("explicit retry did not resume the same native thread once", op.ResumeEvidence, launched, err)
	}
	if stale, saved, err := x.f.store.RecordReplacementResumeUnlaunched(ctx, op.ID, op.Revision-1, rejectedGeneration, "late callback", time.Now()); err != nil || saved || stale.Revision != op.Revision || stale.ResumeEvidence.ResumedAt == nil {
		t.Fatal("late no-launch reply overwrote a newer resume", saved, err)
	}
}
