package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestReplacementIdleTimeoutReleasesSourceIntakeWithoutStopping(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, err := f.svc.Accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementPreflight
	if saved, err := f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	op, _, err = f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementWaitingSourceIdle
	if saved, err := f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	svc := replacement.New(replacement.Deps{Store: f.store, Runtime: f.runtime, Documents: f.docs, Now: func() time.Time { return op.DeadlineAt.Add(time.Second) }})
	result, err := svc.Advance(ctx, op.ID)
	if err != nil || result.Phase != domain.ReplacementAwaitingSource {
		t.Fatal(result.Phase, err)
	}
	fenced, err := f.store.ReplacementWakeFenced(ctx, f.source.ID)
	if err != nil || fenced {
		t.Fatal("idle timeout left unchanged live source intake fenced", fenced, err)
	}
	if f.runtime.quiescences != 0 || f.runtime.retirements != 0 || f.runtime.successors != 0 {
		t.Fatal("timeout changed provider lifecycle")
	}
}

func TestReplacementSourceReleaseUnfreezeRollsBackAndRetryRefences(t *testing.T) {
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
		t.Fatal("fixture not at frozen pre-stop boundary")
	}
	op.Phase = domain.ReplacementAwaitingTranscript
	op.ResumePhase = domain.ReplacementExportingTranscript
	if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	op, _, err = x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER refuse_source_release BEFORE UPDATE ON orchestrator_replacements WHEN json_type(NEW.state_json,'$.sourceIntakeReleasedAt')='text' BEGIN SELECT RAISE(ABORT,'source release ENOSPC'); END`); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := x.f.store.ReleaseReplacementSourceIntake(ctx, op.ID, op.Revision, time.Now()); err == nil || applied {
		t.Fatal("release survived ENOSPC", applied, err)
	}
	state, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || state.Lease == nil || !state.Lease.Frozen {
		t.Fatal("failed release left lease unfrozen", err)
	}
	current, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || current.SourceIntakeReleasedAt != nil || current.Revision != op.Revision {
		t.Fatal("failed release changed operation", err)
	}
	if _, err := raw.Exec(`DROP TRIGGER refuse_source_release`); err != nil {
		t.Fatal(err)
	}
	released, applied, err := x.f.store.ReleaseReplacementSourceIntake(ctx, op.ID, op.Revision, time.Now())
	if err != nil || !applied || released.SourceIntakeReleasedAt == nil {
		t.Fatal(applied, err)
	}
	state, err = x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || state.Lease.Frozen {
		t.Fatal("source writer remained frozen", err)
	}
	if fenced, err := x.f.store.ReplacementWakeFenced(ctx, op.SourceSessionID); err != nil || fenced {
		t.Fatal("released frozen source remains fenced", fenced, err)
	}
	actor := projectdocuments.Actor{SessionID: op.SourceSessionID, Capability: "author-secret"}
	grant, err := x.documents.Acquire(ctx, op.ProjectID, actor)
	if err != nil {
		t.Fatal("released source cannot acquire living writer", err)
	}
	written, err := x.documents.Write(ctx, op.ProjectID, projectdocuments.WriteInput{Actor: actor, Lease: grant, Path: "backlog/after-timeout.md", Content: "source continues after bounded handoff wait", ExpectedHash: projectdocuments.MissingHash})
	if err != nil || written.Content != "source continues after bounded handoff wait" {
		t.Fatal("released source writer remains fenced", written, err)
	}
	retried, err := x.service().Retry(ctx, op.ID)
	if err != nil || retried.SourceIntakeReleasedAt != nil || len(retried.Attempts) != 1 || retried.Attempts[0].SourceIntakeReleasedAt == nil {
		t.Fatal("retry lost release provenance", retried.Phase, err)
	}
	if fenced, err := x.f.store.ReplacementWakeFenced(ctx, op.SourceSessionID); err != nil || !fenced {
		t.Fatal("retry failed to restore source fence", fenced, err)
	}
	after, _, err := x.f.store.GetSession(ctx, x.worker.ID)
	if err != nil || !reflect.DeepEqual(worker, after) {
		t.Fatal("source release changed active worker", err)
	}
	if x.f.runtime.quiescences != 0 || x.f.runtime.retirements != 0 {
		t.Fatal("release/retry stopped source")
	}
}
