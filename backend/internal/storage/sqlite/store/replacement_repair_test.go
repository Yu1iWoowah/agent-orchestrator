package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type replacementOperatorStore interface {
	DiagnoseOrchestratorReplacement(context.Context, string) (domain.ReplacementDiagnostic, bool, error)
	GetReplacementRepairSnapshot(context.Context, string) (domain.ReplacementRepairSnapshot, bool, error)
	RepairOrchestratorReplacement(context.Context, domain.ReplacementRepairRequest, time.Time) (domain.OrchestratorReplacement, bool, error)
}

func repairFixture(t *testing.T) (*sqlite.Store, replacementOperatorStore, domain.OrchestratorReplacement, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dir)
	op, _, err := st.CreateOrchestratorReplacement(context.Background(), replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	api, ok := any(st).(replacementOperatorStore)
	if !ok {
		t.Fatal("malformed active replacement has no verified internal repair path")
	}
	return st, api, op, raw
}

func corruptReplacementRequest(t *testing.T, raw *sql.DB, id string) {
	t.Helper()
	if _, err := raw.Exec(`UPDATE orchestrator_replacements SET request_hash='broken',state_json=json_set(state_json,'$.requestHash','broken') WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func repairInput(t *testing.T, api replacementOperatorStore, id string) domain.ReplacementRepairRequest {
	t.Helper()
	ctx := context.Background()
	diagnostic, found, err := api.DiagnoseOrchestratorReplacement(ctx, id)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	snapshot, found, err := api.GetReplacementRepairSnapshot(ctx, id)
	if err != nil || !found {
		t.Fatal("no known-good snapshot", found, err)
	}
	return domain.ReplacementRepairRequest{OperationID: id, ExpectedRevision: diagnostic.Revision, ExpectedDigest: diagnostic.RawDigest, Snapshot: snapshot}
}

func TestReplacementOperatorRepairAllowsOnlyDataRestoreBeforeCancel(t *testing.T) {
	st, api, op, raw := repairFixture(t)
	ctx := context.Background()
	beforeSession, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeLease, err := st.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	corruptReplacementRequest(t, raw, op.ID)
	if _, _, err := st.GetOrchestratorReplacement(ctx, op.ID); err == nil {
		t.Fatal("malformed operation read as success")
	}
	if _, applied, err := st.ResetReplacementBeforeStop(ctx, op.ID, op.Revision, true, time.Now(), time.Now().Add(time.Hour)); err == nil || applied {
		t.Fatal("broken row bypassed safe repair", applied, err)
	}
	in := repairInput(t, api, op.ID)
	diag, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil || !diag.Corrupt || !diag.Active || diag.ProjectID != op.ProjectID || diag.SourceSessionID != op.SourceSessionID {
		t.Fatal(diag, err)
	}
	repaired, applied, err := api.RepairOrchestratorReplacement(ctx, in, time.Now())
	if err != nil || !applied || repaired.Phase != domain.ReplacementRecoveryRequired || repaired.ResumePhase != "" || repaired.SourceStop != nil || repaired.SourceStopIntentAt != nil || len(repaired.Repairs) != 1 || !repaired.Repairs[0].PreStopSnapshot {
		t.Fatal("unverified recovery result", repaired, applied, err)
	}
	afterLease, err := st.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || !reflect.DeepEqual(beforeLease, afterLease) {
		t.Fatal("repair moved lease", err)
	}
	afterSession, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil || !reflect.DeepEqual(beforeSession, afterSession) {
		t.Fatal("repair changed source", err)
	}
	again, applied, err := api.RepairOrchestratorReplacement(ctx, in, time.Now())
	if err != nil || applied || again.ID != repaired.ID || again.Revision != repaired.Revision {
		t.Fatal("lost reply repeated repair", applied, err)
	}
	cancelled, applied, err := st.ResetReplacementBeforeStop(ctx, repaired.ID, repaired.Revision, true, time.Now(), time.Now().Add(time.Hour))
	if err != nil || !applied || cancelled.Phase != domain.ReplacementCancelled {
		t.Fatal("proven pre-stop restore cannot cancel", applied, err)
	}
}

func TestReplacementRepairRefusesForgedOrStaleSnapshotAndCAS(t *testing.T) {
	for _, which := range []string{"payload", "digest", "snapshot revision", "row revision", "row digest", "stale snapshot"} {
		t.Run(which, func(t *testing.T) {
			st, api, op, raw := repairFixture(t)
			ctx := context.Background()
			saved := repairInput(t, api, op.ID).Snapshot
			if which == "stale snapshot" {
				op.Phase = domain.ReplacementPreflight
				if ok, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			corruptReplacementRequest(t, raw, op.ID)
			in := repairInput(t, api, op.ID)
			switch which {
			case "payload":
				var row map[string]json.RawMessage
				if err := json.Unmarshal(in.Snapshot.Payload, &row); err != nil {
					t.Fatal(err)
				}
				row["IntakeRevision"] = json.RawMessage(`991`)
				in.Snapshot.Payload, _ = json.Marshal(row)
				sum := sha256.Sum256(in.Snapshot.Payload)
				in.Snapshot.Digest = hex.EncodeToString(sum[:])
			case "digest":
				in.Snapshot.Digest = "bad"
			case "snapshot revision":
				in.Snapshot.Revision++
			case "row revision":
				in.ExpectedRevision++
			case "row digest":
				in.ExpectedDigest = "bad"
			case "stale snapshot":
				in.Snapshot = saved
			}
			before, _, _ := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
			if _, applied, err := api.RepairOrchestratorReplacement(ctx, in, time.Now()); applied || err == nil {
				t.Fatal("unverified repair applied", which, applied, err)
			}
			after, _, _ := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
			if before.RawDigest != after.RawDigest || !after.Active {
				t.Fatal("rejection changed reservation")
			}
		})
	}
}

func TestReplacementRepairCannotEraseUnsealedNativeOrPublicationFacts(t *testing.T) {
	for _, field := range []string{"sourceStopIntentAt", "sourceStop", "sourceRetiredAt", "writerActivation", "transcript"} {
		t.Run(field, func(t *testing.T) {
			_, api, op, raw := repairFixture(t)
			ctx := context.Background()
			corruptReplacementRequest(t, raw, op.ID)
			value := `"2026-10-29T00:00:00Z"`
			if field == "sourceStop" || field == "writerActivation" {
				value = `{"owner":"unknown"}`
			}
			if field == "transcript" {
				value = `{"receipt":{"directory":"/unverified"}}`
			}
			if _, err := raw.Exec(`UPDATE orchestrator_replacements SET state_json=json_set(state_json,?,json(?)) WHERE id=?`, "$."+field, value, op.ID); err != nil {
				t.Fatal(err)
			}
			in := repairInput(t, api, op.ID)
			if _, applied, err := api.RepairOrchestratorReplacement(ctx, in, time.Now()); applied || !errors.Is(err, domain.ErrReplacementRepairUnsafe) {
				t.Fatal("repair erased unknown positive fact", field, applied, err)
			}
		})
	}
}

func TestReplacementRepairKeepsStopIntentAndFrozenWriter(t *testing.T) {
	st, api, op, raw := repairFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	op.Phase = domain.ReplacementExportingTranscript
	op.FrozenAt, op.SourceStopIntentAt = &now, &now
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	state := domain.ProjectDocumentState{Lease: &domain.ProjectDocumentLease{SessionID: op.SourceSessionID, Owner: op.SourceOwner, Epoch: 7, ReplacementID: op.ID, Frozen: true}}
	if ok, err := st.SaveProjectDocumentState(ctx, op.ProjectID, state); err != nil || !ok {
		t.Fatal(ok, err)
	}
	before, err := st.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	corruptReplacementRequest(t, raw, op.ID)
	repaired, applied, err := api.RepairOrchestratorReplacement(ctx, repairInput(t, api, op.ID), now)
	if err != nil || !applied || repaired.SourceStopIntentAt == nil || repaired.Repairs[0].PreStopSnapshot {
		t.Fatal(repaired, applied, err)
	}
	after, err := st.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("repair unfroze writer", after, err)
	}
	if _, applied, err := st.ResetReplacementBeforeStop(ctx, op.ID, repaired.Revision, true, now, now.Add(time.Hour)); applied || err != nil {
		t.Fatal("uncertain stop became cancellable", applied, err)
	}
}

func TestReplacementRepairCheckpointFailureRollsBackSaveAndRepair(t *testing.T) {
	for _, action := range []string{"save", "repair"} {
		t.Run(action, func(t *testing.T) {
			st, api, op, raw := repairFixture(t)
			ctx := context.Background()
			if action == "repair" {
				corruptReplacementRequest(t, raw, op.ID)
			}
			in := repairInput(t, api, op.ID)
			if _, err := raw.Exec(`CREATE TRIGGER test_reject_checkpoint BEFORE INSERT ON orchestrator_replacement_repair_snapshots BEGIN SELECT RAISE(ABORT,'test checkpoint unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			if action == "save" {
				op.Phase = domain.ReplacementPreflight
				if applied, err := st.SaveOrchestratorReplacement(ctx, op); err == nil || applied {
					t.Fatal("save escaped snapshot failure", applied, err)
				}
			} else if _, applied, err := api.RepairOrchestratorReplacement(ctx, in, time.Now()); err == nil || applied {
				t.Fatal("repair escaped snapshot failure", applied, err)
			}
			diag, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
			if err != nil || diag.RawDigest != in.ExpectedDigest || diag.Revision != in.ExpectedRevision || !diag.Active {
				t.Fatal("checkpoint failure mutated row", diag, err)
			}
		})
	}
}

func TestReplacementRepairCannotLaunderRawRowsOrForgeRepairReceipts(t *testing.T) {
	for _, action := range []string{"raw status", "first receipt", "drop receipt", "change pre-stop proof"} {
		t.Run(action, func(t *testing.T) {
			st, api, op, raw := repairFixture(t)
			ctx := context.Background()
			switch action {
			case "raw status":
				if _, err := raw.Exec(`UPDATE orchestrator_replacements SET state_json=json_set(state_json,'$.error',json('{"code":"raw","message":"raw","retryable":true}')) WHERE id=?`, op.ID); err != nil {
					t.Fatal(err)
				}
			case "first receipt":
				op.Repairs = []domain.ReplacementRepairReceipt{{BeforeRevision: 1, BeforeDigest: op.RequestHash, SnapshotRevision: 1, SnapshotDigest: op.RequestHash, AfterRevision: 2, PreStopSnapshot: true, RepairedAt: time.Now()}}
			default:
				corruptReplacementRequest(t, raw, op.ID)
				var err error
				op, _, err = api.RepairOrchestratorReplacement(ctx, repairInput(t, api, op.ID), time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if action == "drop receipt" {
					op.Repairs = nil
				} else {
					op.Repairs[0].PreStopSnapshot = false
				}
			}
			before, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if applied, err := st.SaveOrchestratorReplacement(ctx, op); applied || err == nil {
				t.Fatal("generic save authenticated altered data", applied, err)
			}
			after, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
			if err != nil || after.RawDigest != before.RawDigest {
				t.Fatal("rejected generic save changed row", after, err)
			}
		})
	}
}

func TestReplacementRepairRequiresTrustedSnapshotAndDoesNotSealDiagnosticReads(t *testing.T) {
	st, api, op, raw := repairFixture(t)
	ctx := context.Background()
	// Simulate a legacy reservation created before the repair-snapshot migration.
	// The product has no delete API; this isolated fixture explicitly removes it.
	if _, err := raw.Exec(`DROP TRIGGER replacement_repair_snapshot_no_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM orchestrator_replacement_repair_snapshots WHERE operation_id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	corruptReplacementRequest(t, raw, op.ID)
	diag, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil || !diag.Corrupt || !diag.Active {
		t.Fatal(diag, err)
	}
	if _, found, err := api.GetReplacementRepairSnapshot(ctx, op.ID); found || err != nil {
		t.Fatal("diagnostic minted a snapshot", found, err)
	}
	if _, _, err := st.GetOrchestratorReplacement(ctx, op.ID); err == nil {
		t.Fatal("unsealed corrupted row read as valid")
	}
	if _, applied, err := api.RepairOrchestratorReplacement(ctx, domain.ReplacementRepairRequest{OperationID: op.ID, ExpectedRevision: diag.Revision, ExpectedDigest: diag.RawDigest, Snapshot: domain.ReplacementRepairSnapshot{OperationID: op.ID, Digest: op.RequestHash}}, time.Now()); applied || !errors.Is(err, domain.ErrReplacementRepairUnverified) {
		t.Fatal(applied, err)
	}
	after, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil || after.RawDigest != diag.RawDigest || !after.Active {
		t.Fatal(after, err)
	}
}

func TestReplacementRepairSnapshotsAreImmutable(t *testing.T) {
	_, _, op, raw := repairFixture(t)
	for _, statement := range []string{`UPDATE orchestrator_replacement_repair_snapshots SET digest='bad' WHERE operation_id=?`, `DELETE FROM orchestrator_replacement_repair_snapshots WHERE operation_id=?`} {
		if _, err := raw.Exec(statement, op.ID); err == nil {
			t.Fatal("sealed snapshot was mutable", statement)
		}
	}
}

func TestReplacementRepairRejectsAmbiguousDuplicateFacts(t *testing.T) {
	_, api, op, raw := repairFixture(t)
	ctx := context.Background()
	corruptReplacementRequest(t, raw, op.ID)
	if _, err := raw.Exec(`UPDATE orchestrator_replacements SET state_json=substr(state_json,1,length(state_json)-1)||',"sourceStop":{"nativeId":"unknown"},"sourceStop":null}' WHERE id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	in := repairInput(t, api, op.ID)
	if _, applied, err := api.RepairOrchestratorReplacement(ctx, in, time.Now()); applied || !errors.Is(err, domain.ErrReplacementRepairUnsafe) {
		t.Fatal("duplicate key hid a native fact", applied, err)
	}
}

func TestReplacementSuccessorBindingRollsBackWhenCheckpointCannotCommit(t *testing.T) {
	st, api, op, raw := repairFixture(t)
	ctx := context.Background()
	op.Phase = domain.ReplacementStarting
	op.SpawnRequestID, op.SpawnRequestHash = "replacement/"+op.ID, op.RequestHash
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	source, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	source.IsTerminated = true
	if err := st.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	in := repairInput(t, api, op.ID)
	if _, err := raw.Exec(`CREATE TRIGGER test_reject_binding_checkpoint BEFORE INSERT ON orchestrator_replacement_repair_snapshots BEGIN SELECT RAISE(ABORT,'test checkpoint unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	fresh := sampleRecord(string(op.ProjectID))
	fresh.Kind, fresh.Mode = domain.KindOrchestrator, domain.SessionModeChat
	fresh.ClientRequestID, fresh.ClientRequestHash = op.SpawnRequestID, op.SpawnRequestHash
	if _, created, err := st.CreateClientRequestSession(ctx, fresh); created || err == nil {
		t.Fatal("successor escaped snapshot failure", created, err)
	}
	if _, found, err := st.GetSessionByClientRequestID(ctx, fresh.ClientRequestID); found || err != nil {
		t.Fatal("failed binding left a session", found, err)
	}
	diag, _, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
	if err != nil || diag.RawDigest != in.ExpectedDigest || diag.Revision != in.ExpectedRevision || diag.Corrupt {
		t.Fatal("binding failure changed operation", diag, err)
	}
}
