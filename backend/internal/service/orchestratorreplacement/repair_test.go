package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

type operatorService interface {
	Diagnose(context.Context, string) (domain.ReplacementDiagnostic, error)
	RepairSnapshot(context.Context, string) (domain.ReplacementRepairSnapshot, error)
	Repair(context.Context, domain.ReplacementRepairRequest) (domain.OrchestratorReplacement, error)
}

func TestReplacementOperatorRepairIsDataOnlyAndUsesTrustedProjectGate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, err := f.svc.Accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	var gates []domain.ProjectID
	svc := replacement.New(replacement.Deps{Store: f.store, Runtime: f.runtime, Documents: f.docs, WithProjectGate: func(ctx context.Context, id domain.ProjectID, fn func(context.Context) error) error {
		gates = append(gates, id)
		return fn(ctx)
	}})
	api, ok := any(svc).(operatorService)
	if !ok {
		t.Fatal("internal operator repair service is unavailable")
	}
	snapshot, err := api.RepairSnapshot(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	// The raw project is diagnostic data, never authority for the project gate.
	damagedProject := f.project
	damagedProject.ID = "untrusted"
	if err := f.store.UpsertProject(ctx, damagedProject); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE orchestrator_replacements SET project_id='untrusted',request_hash='damaged', state_json=json_set(state_json,'$.requestHash','damaged','$.projectId','untrusted') WHERE id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	diag, err := api.Diagnose(ctx, op.ID)
	if err != nil || !diag.Corrupt {
		t.Fatal(diag, err)
	}
	if len(gates) != 0 {
		t.Fatal("read-only diagnostic acquired write gate")
	}
	preflights, observations, documents := f.runtime.preflights, f.runtime.observations, f.docs.calls
	repaired, err := api.Repair(ctx, domain.ReplacementRepairRequest{OperationID: op.ID, ExpectedRevision: diag.Revision, ExpectedDigest: diag.RawDigest, Snapshot: snapshot})
	if err != nil || repaired.Phase != domain.ReplacementRecoveryRequired || len(gates) != 1 || gates[0] != domain.ProjectID(f.project.ID) {
		t.Fatal(repaired, gates, err)
	}
	if _, err := svc.Advance(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if f.runtime.preflights != preflights || f.runtime.observations != observations || f.runtime.quiescences != 0 || f.runtime.preparations != 0 || f.docs.calls != documents {
		t.Fatal("data restore performed provider/document actions")
	}
	if _, err := svc.Cancel(ctx, op.ID); err != nil {
		t.Fatal("proven pre-stop repair cannot use existing cancel", err)
	}
}
