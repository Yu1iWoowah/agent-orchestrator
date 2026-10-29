package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestReplacementPendingWorkerReportsInvalidatePreparationButDeliveryBookkeepingDoesNot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dir)
	op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.CreateSession(ctx, sampleRecord("replace"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	assertRevision := func(want int64) {
		t.Helper()
		current, _, err := st.GetOrchestratorReplacement(ctx, op.ID)
		if err != nil || current.IntakeRevision != want || current.Revision != op.Revision {
			t.Fatalf("report cutoff intake=%d want=%d saga=%d/%d err=%v", current.IntakeRevision, want, current.Revision, op.Revision, err)
		}
	}
	report := domain.ReportRecord{ID: "worker-question", SessionID: worker.ID, ProjectID: op.ProjectID, State: domain.ReportNeedsInput, Note: "Q12: which option?", CreatedAt: now, AvailableAt: now, DeliveryState: domain.ReportPending, RepeatCount: 1}
	if _, err := st.CreateReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	assertRevision(1)
	if _, ok, err := st.ClaimReport(ctx, report.ID, "claim", now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	assertRevision(1)
	if _, ok, err := st.ReleaseReport(ctx, report.ID, "claim", now, "replacement busy"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, ok, err := st.ClaimReport(ctx, report.ID, "claim-again", now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, ok, err := st.AcknowledgeReport(ctx, report.ID, "claim-again", now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := st.RequeueClaimedReports(ctx); err != nil {
		t.Fatal(err)
	}
	assertRevision(1)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for i, query := range []string{
		"UPDATE reports SET note='Q13: new question' WHERE id='worker-question'",
		"UPDATE reports SET message='new directive acknowledgement', state='', note='' WHERE id='worker-question'",
		"DELETE FROM reports WHERE id='worker-question'",
	} {
		if _, err := raw.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
		assertRevision(int64(i + 2))
	}
	self := report
	self.ID, self.SessionID = "source-report", op.SourceSessionID
	if _, err := st.CreateReport(ctx, self); err != nil {
		t.Fatal(err)
	}
	assertRevision(4)
	op.FrozenAt = &now
	op.Phase = domain.ReplacementExportingTranscript
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	report.ID = "after-cutoff"
	if _, err := st.CreateReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	assertRevision(4)
}
