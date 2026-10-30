package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func replacementSpawnIntent(t *testing.T, dir string) (*sqlite.Store, domain.OrchestratorReplacement, domain.SessionRecord) {
	t.Helper()
	ctx := context.Background()
	store, op, source, state := replacementRetirementFixture(t, dir, "")
	op, applied, err := store.CommitReplacementRetirement(ctx, op.ID, op.Revision, source, state.Revision, func(ctx context.Context) error { return ctx.Err() })
	if err != nil || !applied {
		t.Fatal(applied, err)
	}
	op.SpawnRequestID, op.SpawnRequestHash = "replacement:"+op.ID+":successor", strings.Repeat("a", 64)
	if applied, err := store.SaveOrchestratorReplacement(ctx, op); err != nil || !applied {
		t.Fatal(applied, err)
	}
	op, _, err = store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}, ClientRequestID: op.SpawnRequestID, ClientRequestHash: op.SpawnRequestHash}
	return store, op, seed
}

func TestReplacementSuccessorBindingFailureRollsBackSeedInsert(t *testing.T) {
	dir := t.TempDir()
	store, intent, seed := replacementSpawnIntent(t, dir)
	ctx := context.Background()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER reject_successor_binding BEFORE UPDATE ON orchestrator_replacements WHEN json_type(NEW.state_json,'$.successorSessionId') IS 'text' BEGIN SELECT RAISE(ABORT,'binding ENOSPC'); END`); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := store.CreateClientRequestSession(ctx, seed); err == nil || fresh {
		t.Fatal("binding failure accepted a successor", fresh, err)
	}
	if _, found, err := store.GetSessionByClientRequestID(ctx, seed.ClientRequestID); err != nil || found {
		t.Fatal("binding failure left an unbound seed", err)
	}
	op, _, err := store.GetOrchestratorReplacement(ctx, intent.ID)
	if err != nil || op.Revision != intent.Revision || op.SuccessorSessionID != "" {
		t.Fatal("binding failure leaked operation state", err)
	}
	if _, err := raw.ExecContext(ctx, "DROP TRIGGER reject_successor_binding"); err != nil {
		t.Fatal(err)
	}
	created, fresh, err := store.CreateClientRequestSession(ctx, seed)
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	op, _, err = store.GetOrchestratorReplacement(ctx, intent.ID)
	if err != nil || op.SuccessorSessionID != created.ID {
		t.Fatal("same intent could not recover after rollback", err)
	}
}

func TestReplacementSuccessorTwoConnectionsChooseOneSeed(t *testing.T) {
	dir := t.TempDir()
	store, intent, seed := replacementSpawnIntent(t, dir)
	other, err := sqlite.OpenPreMigrated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	type result struct {
		rec   domain.SessionRecord
		fresh bool
		err   error
	}
	start, results := make(chan struct{}), make(chan result, 2)
	for _, db := range []*sqlite.Store{store, other} {
		go func() {
			<-start
			rec, fresh, err := db.CreateClientRequestSession(context.Background(), seed)
			results <- result{rec: rec, fresh: fresh, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.fresh == second.fresh || first.rec.ID != second.rec.ID {
		t.Fatalf("competing seeds did not converge: first id=%s fresh=%t err=%v; second id=%s fresh=%t err=%v", first.rec.ID, first.fresh, first.err, second.rec.ID, second.fresh, second.err)
	}
	op, _, err := store.GetOrchestratorReplacement(context.Background(), intent.ID)
	if err != nil || op.SuccessorSessionID != first.rec.ID || op.Revision != intent.Revision+1 {
		t.Fatal("competing insertion changed operation binding twice", err)
	}
	worker := sampleRecord(string(op.ProjectID))
	worker.Kind = domain.KindWorker
	created, err := store.CreateSession(context.Background(), worker)
	if err != nil {
		t.Fatal("successor reservation blocked a worker", err)
	}
	if _, err := store.DeleteSession(context.Background(), created.ID); err != nil {
		t.Fatal("successor deletion fence blocked a worker", err)
	}
}

func TestReplacementSuccessorInsertAtomicallyBindsOperation(t *testing.T) {
	dir := t.TempDir()
	store, intent, seed := replacementSpawnIntent(t, dir)
	ctx := context.Background()
	created, fresh, err := store.CreateClientRequestSession(ctx, seed)
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	other, err := sqlite.OpenPreMigrated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	op, found, err := other.GetOrchestratorReplacement(ctx, intent.ID)
	if err != nil || !found || op.SuccessorSessionID != created.ID || op.Revision != intent.Revision+1 {
		t.Fatalf("seed returned before operation binding: successor=%s created=%s revision=%d before=%d err=%v", op.SuccessorSessionID, created.ID, op.Revision, intent.Revision, err)
	}
	again, fresh, err := other.CreateClientRequestSession(ctx, seed)
	if err != nil || fresh || again.ID != created.ID {
		t.Fatal("retry created another successor", fresh, err)
	}
	intent.UpdatedAt = time.Now().UTC()
	if applied, err := store.SaveOrchestratorReplacement(ctx, intent); err != nil || applied {
		t.Fatal("stale coordinator overwrote seed binding", applied, err)
	}
}

func TestReplacementSuccessorSeedCannotBeDeletedAndRecreated(t *testing.T) {
	for _, phase := range []domain.ReplacementPhase{domain.ReplacementStarting, domain.ReplacementRecoveryRequired} {
		t.Run(string(phase), func(t *testing.T) {
			store, intent, seed := replacementSpawnIntent(t, t.TempDir())
			ctx := context.Background()
			created, fresh, err := store.CreateClientRequestSession(ctx, seed)
			if err != nil || !fresh {
				t.Fatal(fresh, err)
			}
			op, _, err := store.GetOrchestratorReplacement(ctx, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = phase
			if applied, err := store.SaveOrchestratorReplacement(ctx, op); err != nil || !applied {
				t.Fatal(applied, err)
			}
			if _, err := store.DeleteSession(ctx, created.ID); err == nil {
				t.Fatal("rollback could delete replacement successor seed")
			}
			again, fresh, err := store.CreateClientRequestSession(ctx, seed)
			if err != nil || fresh || again.ID != created.ID {
				t.Fatal("failed seed replay changed successor", fresh, err)
			}
		})
	}
}

func TestReplacementSuccessorPrelaunchFailureCannotOverwriteClaimedOwner(t *testing.T) {
	for _, change := range []string{"unclaimed", "native claimed", "generation claimed", "operation waiting"} {
		t.Run(change, func(t *testing.T) {
			store, intent, seed := replacementSpawnIntent(t, t.TempDir())
			ctx := context.Background()

			rec, _, err := store.CreateClientRequestSession(ctx, seed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetSessionProvisionState(ctx, rec.ID, domain.SessionProvisionProvisioning, "", time.Now()); err != nil {
				t.Fatal(err)
			}
			rec, _, err = store.GetSession(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, rec, err = store.BeginReplacementSpawnAttempt(ctx, intent.ID, rec.ID, "attempt-a", time.Now()); err != nil {
				t.Fatal(err)
			}

			switch change {
			case "native claimed":
				rec.Metadata.ProviderConversationID = "claimed-native"
			case "generation claimed":
				rec.Metadata.ControllerGeneration = "claimed-generation"
			case "operation waiting":
				op, _, err := store.GetOrchestratorReplacement(ctx, intent.ID)
				if err != nil {
					t.Fatal(err)
				}
				op.Phase = domain.ReplacementRecoveryRequired
				if applied, err := store.SaveOrchestratorReplacement(ctx, op); err != nil || !applied {
					t.Fatal(applied, err)
				}
			}
			if err := store.UpdateSession(ctx, rec); err != nil {
				t.Fatal(err)
			}
			applied, err := store.FailReplacementSpawnAttempt(ctx, intent.ID, rec.ID, "attempt-a", "prelaunch configuration changed", time.Now())
			if applied != (change == "unclaimed") || change == "unclaimed" && err != nil {
				t.Fatalf("%s: applied=%t err=%v", change, applied, err)
			}
			after, _, err := store.GetSession(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if change == "unclaimed" {
				if after.ProvisionState != domain.SessionProvisionFailed || after.ProvisionError != "prelaunch configuration changed" {
					t.Fatal("prelaunch failure not recorded")
				}
			} else if after.ProvisionState != domain.SessionProvisionProvisioning || after.Metadata.ProviderConversationID != rec.Metadata.ProviderConversationID || after.Metadata.ControllerGeneration != rec.Metadata.ControllerGeneration {
				t.Fatal("stale prelaunch failure overwrote current owner")
			}
		})
	}
}
