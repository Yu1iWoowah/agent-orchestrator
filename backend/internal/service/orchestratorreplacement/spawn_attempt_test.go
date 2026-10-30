package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func spawnAttemptFixture(t *testing.T) (*preparedExport, domain.OrchestratorReplacement, domain.SessionRecord) {
	t.Helper()
	x := retiredExportFixture(t)
	ctx := context.Background()
	op, err := x.service().Advance(ctx, x.operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed, _, err := x.f.store.CreateClientRequestSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, ClientRequestID: op.SpawnRequestID, ClientRequestHash: op.SpawnRequestHash, CreatedAt: now, UpdatedAt: now, Metadata: domain.SessionMetadata{Model: op.TargetAgentConfig.Model, Effort: op.TargetAgentConfig.Effort, Permissions: op.TargetAgentConfig.Permissions}})
	if err != nil {
		t.Fatal(err)
	}
	op, _, err = x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return x, op, seed
}

func TestReplacementSpawnAttemptReceiptSurvivesStoreRestart(t *testing.T) {
	x, op, seed := spawnAttemptFixture(t)
	ctx := context.Background()
	if _, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	if applied, err := x.f.store.MarkReplacementSpawnLaunch(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
	before, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.OpenPreMigrated(x.f.store.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, found, err := reopened.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || !found || after.SuccessorSessionID != seed.ID || !reflect.DeepEqual(before.SpawnAttempts, after.SpawnAttempts) {
		t.Fatal("restart lost launch/identity proof", found, err)
	}
	if applied, err := reopened.MarkReplacementSpawnLaunch(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err != nil || applied {
		t.Fatal("restart replayed launch permission", applied, err)
	}
}

func authorizeSpawnRetry(t *testing.T, x *preparedExport, id string) domain.OrchestratorReplacement {
	t.Helper()
	ctx := context.Background()
	op, _, err := x.f.store.GetOrchestratorReplacement(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase, op.ResumePhase = domain.ReplacementRecoveryRequired, domain.ReplacementStarting
	if applied, err := x.f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !applied {
		t.Fatal(applied, err)
	}
	retried, err := x.service().Retry(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return retried
}

func TestReplacementSpawnAttemptLaunchIsSingleUseAndReceiptsImmutable(t *testing.T) {
	x, op, seed := spawnAttemptFixture(t)
	ctx := context.Background()
	reserved, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-a", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-b", time.Now()); err == nil {
		t.Fatal("polling replaced the reserved attempt")
	}
	if applied, err := x.f.store.MarkReplacementSpawnLaunch(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
	if applied, err := x.f.store.MarkReplacementSpawnLaunch(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err != nil || applied {
		t.Fatal("launch permission replayed", applied, err)
	}
	current, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	forged := current
	forged.SpawnAttempts = append([]domain.ReplacementSpawnAttempt(nil), current.SpawnAttempts...)
	forged.SpawnAttempts[0].LaunchIntentAt = nil
	if _, err := x.f.store.SaveOrchestratorReplacement(ctx, forged); err == nil {
		t.Fatal("generic save erased launch intent")
	}
	if applied, err := x.f.store.FailReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-a", "unknown provider start outcome", time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
	retry := authorizeSpawnRetry(t, x, op.ID)
	if retry.SpawnRetryGeneration != reserved.SpawnRetryGeneration {
		t.Fatal("launch uncertainty authorized a new attempt")
	}
	if _, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-b", time.Now()); err == nil {
		t.Fatal("failed launch intent retried a provider")
	}
}

func TestReplacementSpawnAttemptExplicitRetryKeepsSeedAndFencesOldBackground(t *testing.T) {
	x, op, seed := spawnAttemptFixture(t)
	ctx := context.Background()
	sourceBefore, _, err := x.f.store.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	if applied, err := x.f.store.FailReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-a", "workspace preparation failed", time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
	failed, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-b", time.Now()); err == nil {
		t.Fatal("failure itself authorized background retry")
	}
	retry := authorizeSpawnRetry(t, x, op.ID)
	if retry.SuccessorSessionID != seed.ID || retry.SpawnRetryGeneration != 1 {
		t.Fatal("retry changed bound identity or missed authorization", retry.SpawnRetryGeneration)
	}
	reserved, target, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-b", time.Now())
	if err != nil || target.ID != seed.ID || len(reserved.SpawnAttempts) != 2 || !reflect.DeepEqual(failed.SpawnAttempts[0], reserved.SpawnAttempts[0]) {
		t.Fatal("new attempt lost previous failure or seed", err)
	}
	if applied, err := x.f.store.MarkReplacementSpawnLaunch(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err == nil || applied {
		t.Fatal("old attempt retained launch permission", applied, err)
	}
	if applied, err := x.f.store.FailReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-a", "late old error", time.Now()); err == nil || applied {
		t.Fatal("old error overwrote retry", applied, err)
	}
	sourceAfter, _, err := x.f.store.GetSession(ctx, op.SourceSessionID)
	if err != nil || !reflect.DeepEqual(sourceBefore, sourceAfter) {
		t.Fatal("retry rewrote retired source", err)
	}
	if applied, err := x.f.store.MarkReplacementSpawnLaunch(ctx, op.ID, seed.ID, "attempt-b", time.Now()); err != nil || !applied {
		t.Fatal(applied, err)
	}
}

func TestReplacementSpawnAttemptCASAndENOSPCRollBackBeforeLaunch(t *testing.T) {
	for _, fault := range []string{"reserve CAS", "launch ENOSPC"} {
		t.Run(fault, func(t *testing.T) {
			x, op, seed := spawnAttemptFixture(t)
			ctx := context.Background()
			raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if fault == "reserve CAS" {
				if _, err := raw.Exec(`CREATE TRIGGER race_spawn_reserve BEFORE UPDATE OF provision_state ON sessions BEGIN UPDATE orchestrator_replacements SET revision=revision+1; END`); err != nil {
					t.Fatal(err)
				}
				if _, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err == nil {
					t.Fatal("failed CAS reserved an attempt")
				}
			} else {
				if _, _, err := x.f.store.BeginReplacementSpawnAttempt(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Exec(`CREATE TRIGGER fail_spawn_launch BEFORE UPDATE ON orchestrator_replacements WHEN json_type(NEW.state_json,'$.spawnAttempts[0].launchIntentAt')='text' BEGIN SELECT RAISE(ABORT,'launch ENOSPC'); END`); err != nil {
					t.Fatal(err)
				}
				if applied, err := x.f.store.MarkReplacementSpawnLaunch(ctx, op.ID, seed.ID, "attempt-a", time.Now()); err == nil || applied {
					t.Fatal("failed launch publication authorized provider", applied, err)
				}
			}
			current, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "reserve CAS" && (current.Revision != op.Revision || len(current.SpawnAttempts) != 0) {
				t.Fatal("reserve rollback left partial receipt")
			}
			if attempt, exists := current.LatestSpawnAttempt(); exists && attempt.LaunchIntentAt != nil {
				t.Fatal("failed publication left launch intent")
			}
		})
	}
}
