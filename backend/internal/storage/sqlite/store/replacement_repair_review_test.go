package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementUnsealedUnknownFactsCannotBeSavedOrCancelled(t *testing.T) {
	for _, path := range []string{"$.futureNativeStop", "$.sourceOwner.futureNativeStop"} {
		for _, action := range []string{"save", "cancel"} {
			t.Run(path+"/"+action, func(t *testing.T) {
				st, api, op, raw := repairFixture(t)
				ctx := context.Background()
				// Isolated legacy-upgrade fixture: the product cannot delete capsules.
				if _, err := raw.Exec(`DROP TRIGGER replacement_repair_snapshot_no_delete`); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`DELETE FROM orchestrator_replacement_repair_snapshots WHERE operation_id=?`, op.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`UPDATE orchestrator_replacements SET state_json=json_set(state_json,?,json('{"nativeId":"unknown-native","stopSent":true}')) WHERE id=?`, path, op.ID); err != nil {
					t.Fatal(err)
				}
				diag, found, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID)
				if err != nil || !found || !diag.Corrupt {
					t.Errorf("unknown positive fact read as healthy: found=%t corrupt=%t err=%v", found, diag.Corrupt, err)
				}
				if _, found, err := st.GetOrchestratorReplacement(ctx, op.ID); found || err == nil {
					t.Errorf("unsealed unknown fact admitted: found=%t err=%v", found, err)
				}
				if action == "save" {
					op.Phase = domain.ReplacementPreflight
					if applied, err := st.SaveOrchestratorReplacement(ctx, op); applied || err == nil {
						t.Errorf("generic save laundered unknown fact: applied=%t err=%v", applied, err)
					}
				} else if _, applied, err := st.ResetReplacementBeforeStop(ctx, op.ID, op.Revision, true, time.Now(), time.Now().Add(time.Hour)); applied || err == nil {
					t.Errorf("unknown native fact cancelled: applied=%t err=%v", applied, err)
				}
				var present, snapshots int
				if err := raw.QueryRow(`SELECT json_type(state_json,?) IS NOT NULL FROM orchestrator_replacements WHERE id=?`, path, op.ID).Scan(&present); err != nil {
					t.Fatal(err)
				}
				if err := raw.QueryRow(`SELECT count(*) FROM orchestrator_replacement_repair_snapshots WHERE operation_id=?`, op.ID).Scan(&snapshots); err != nil {
					t.Fatal(err)
				}
				if present != 1 || snapshots != 0 {
					t.Errorf("unknown fact erased or authenticated: present=%d snapshots=%d", present, snapshots)
				}
				if fenced, err := st.ReplacementWakeFenced(ctx, op.SourceSessionID); err != nil || !fenced {
					t.Fatalf("unverified legacy data released source: fenced=%t err=%v", fenced, err)
				}
			})
		}
	}
}

func TestReplacementKnownEarlyLegacyRowCanBootstrap(t *testing.T) {
	st, api, op, raw := repairFixture(t)
	ctx := context.Background()
	if _, err := raw.Exec(`DROP TRIGGER replacement_repair_snapshot_no_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM orchestrator_replacement_repair_snapshots WHERE operation_id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	if diag, found, err := api.DiagnoseOrchestratorReplacement(ctx, op.ID); err != nil || !found || diag.Corrupt || diag.SnapshotRevision != 0 {
		t.Fatal("known early legacy row rejected or diagnostic minted proof", diag, found, err)
	}
	op.Phase = domain.ReplacementPreflight
	if saved, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal("known no-proof row cannot take a legitimate step", saved, err)
	}
	if got, found, err := st.GetOrchestratorReplacement(ctx, op.ID); err != nil || !found || got.Phase != domain.ReplacementPreflight || got.Revision != op.Revision+1 {
		t.Fatal(got, found, err)
	}
}

func TestReplacementRepairRejectsHealthyOperation(t *testing.T) {
	for _, phase := range []domain.ReplacementPhase{domain.ReplacementAccepted, domain.ReplacementStarting, domain.ReplacementNotifying} {
		t.Run(string(phase), func(t *testing.T) {
			st, api, op, _ := repairFixture(t)
			if phase != domain.ReplacementAccepted {
				f := newCanonicalIntakeFixture(t, false, phase == domain.ReplacementNotifying)
				st, op = f.st, f.op
				api = any(st).(replacementOperatorStore)
				if phase == domain.ReplacementNotifying {
					if _, applied, err := f.api.ReleaseReplacementIntake(context.Background(), op.ID, op.Revision, time.Now()); err != nil || !applied {
						t.Fatal(applied, err)
					}
				}
			}
			ctx := context.Background()
			before, found, err := st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			in := repairInput(t, api, op.ID)
			if _, applied, err := api.RepairOrchestratorReplacement(ctx, in, time.Now()); applied || !errors.Is(err, domain.ErrReplacementRepairConflict) {
				t.Errorf("healthy operation was repaired: applied=%t err=%v", applied, err)
			}
			after, found, err := st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil || !found || !reflect.DeepEqual(before, after) {
				t.Fatalf("healthy repair changed durable operation: before=%s/rev%d after=%s/rev%d err=%v", before.Phase, before.Revision, after.Phase, after.Revision, err)
			}
			capsule, found, err := api.GetReplacementRepairSnapshot(ctx, op.ID)
			if err != nil || !found || !reflect.DeepEqual(in.Snapshot, capsule) {
				t.Fatal("healthy repair created a new capsule", found, err)
			}
		})
	}
}
