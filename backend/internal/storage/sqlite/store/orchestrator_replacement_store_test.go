package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func replacementOperation(t *testing.T, st *sqlite.Store) domain.OrchestratorReplacement {
	t.Helper()
	seedProject(t, st, "replace")
	source := sampleRecord("replace")
	source.Kind, source.Mode = domain.KindOrchestrator, domain.SessionModeChat
	source.Metadata.ControllerGeneration = "source-generation"
	source.Metadata.BrowserCapabilityVerifier = "verifier-before-rotation"
	source.Metadata.ProviderConversationID = "native-source"
	source, err := st.CreateSession(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	return domain.OrchestratorReplacement{
		SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111",
		OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64),
		ProjectID: "replace", SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(),
		SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier,
		Phase:                    domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(30 * time.Minute),
	}
}

func TestReplacementStoreIdempotencyAndConcurrency(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	op := replacementOperation(t, st)
	first, created, err := st.CreateOrchestratorReplacement(ctx, op)
	if err != nil || !created || first.Revision != 1 {
		t.Fatalf("durable acceptance: created=%t op=%+v err=%v", created, first, err)
	}
	op.ID = "33333333-3333-4333-8333-333333333333"
	again, created, err := st.CreateOrchestratorReplacement(ctx, op)
	if err != nil || created || again.ID != first.ID || again.Revision != first.Revision {
		t.Fatalf("same key created another operation: %+v %t %v", again, created, err)
	}
	op.RequestHash = strings.Repeat("b", 64)
	if _, _, err = st.CreateOrchestratorReplacement(ctx, op); !errors.Is(err, domain.ErrReplacementKeyConflict) {
		t.Fatalf("different request reused key: %v", err)
	}
	op.OperationKey = "44444444-4444-4444-8444-444444444444"
	winner, created, err := st.CreateOrchestratorReplacement(ctx, op)
	if !errors.Is(err, domain.ErrReplacementInProgress) || created || winner.ID != first.ID {
		t.Fatalf("competing key accepted: %+v %t %v", winner, created, err)
	}
	active, found, err := st.GetActiveOrchestratorReplacement(ctx, op.ProjectID)
	if err != nil || !found || active.ID != first.ID {
		t.Fatalf("active reservation changed: %+v %t %v", active, found, err)
	}
}

func TestReplacementStoreCASAndCrashRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlitetest.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	stale := op
	op.Phase = domain.ReplacementPreflight
	op.SpawnRequestID = "replacement/" + op.ID
	op.Error = &domain.ReplacementFailure{Code: "PREPARATION_PENDING", Message: "waiting for owned turn", Retryable: true}
	op.ResumeEvidence = &domain.ReplacementResumeEvidence{BeforeNativeID: "native-successor"}
	if changed, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !changed {
		t.Fatalf("advance: changed=%t err=%v", changed, err)
	}
	stale.Phase = domain.ReplacementPreflight
	if changed, err := st.SaveOrchestratorReplacement(ctx, stale); err != nil || changed {
		t.Fatalf("stale revision overwrote progress: changed=%t err=%v", changed, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = sqlite.OpenPreMigrated(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || !found || got.Revision != 2 || got.Phase != op.Phase || got.SpawnRequestID != op.SpawnRequestID || !reflect.DeepEqual(got.Error, op.Error) || !reflect.DeepEqual(got.ResumeEvidence, op.ResumeEvidence) {
		t.Fatalf("restart lost durable facts: %+v found=%t err=%v", got, found, err)
	}
	ready, err := st.ListActiveOrchestratorReplacements(ctx)
	if err != nil || len(ready) != 1 || ready[0].ID != op.ID {
		t.Fatalf("restart discovery: %+v %v", ready, err)
	}
	got.RequestHash = strings.Repeat("c", 64)
	if changed, err := st.SaveOrchestratorReplacement(ctx, got); err == nil || changed {
		t.Fatal("CAS changed immutable request identity")
	}
}

func TestReplacementStoreCannotRebindPreflightAccount(t *testing.T) {
	st := newTestStore(t)
	op := replacementOperation(t, st)
	op.TargetAccountID = "validated-account"
	op, _, err := st.CreateOrchestratorReplacement(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	op.TargetAccountID = "different-account"
	if changed, err := st.SaveOrchestratorReplacement(context.Background(), op); !errors.Is(err, domain.ErrReplacementStateInvalid) || changed {
		t.Fatalf("changed pinned account: saved=%t err=%v", changed, err)
	}
}

func TestReplacementStoreCrossConnectionAdmission(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := sqlitetest.MustOpenAt(t, dir)
	op := replacementOperation(t, first)
	second, err := sqlite.OpenPreMigrated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	start := make(chan struct{})
	results := make(chan bool, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate := op
			candidate.ID = fmt.Sprintf("%08d-0000-4000-8000-000000000000", i+1)
			candidate.OperationKey = candidate.ID
			st := first
			if i%2 != 0 {
				st = second
			}
			<-start
			_, created, err := st.CreateOrchestratorReplacement(ctx, candidate)
			if err != nil && !errors.Is(err, domain.ErrReplacementInProgress) {
				t.Errorf("admission: %v", err)
			}
			results <- created
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for created := range results {
		if created {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("cross-connection winners=%d, want one", winners)
	}
}

func TestReplacementStoreCannotRebindSuccessorOrReopenClosedOperation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	op.SpawnRequestID, op.SpawnRequestHash, op.SuccessorSessionID = "replacement/"+op.ID, strings.Repeat("d", 64), "replace-successor"
	if ok, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, field := range []string{"successor", "spawn key", "spawn hash", "source owner", "source verifier"} {
		t.Run(field, func(t *testing.T) {
			changed, found, err := st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			switch field {
			case "successor":
				changed.SuccessorSessionID = "different-successor"
			case "spawn key":
				changed.SpawnRequestID = "different-intent"
			case "spawn hash":
				changed.SpawnRequestHash = strings.Repeat("e", 64)
			case "source owner":
				changed.SourceOwner.ControllerGeneration = "different-generation"
			case "source verifier":
				changed.SourceCapabilityVerifier = "different-verifier"
			}
			if ok, err := st.SaveOrchestratorReplacement(ctx, changed); err == nil || ok {
				t.Fatal("a bound successor identity was silently replaced")
			}
		})
	}
	bound, _, err := st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	bound.Phase = domain.ReplacementCancelled
	if ok, err := st.SaveOrchestratorReplacement(ctx, bound); err != nil || !ok {
		t.Fatal(ok, err)
	}
	closed, _, err := st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	closed.Phase = domain.ReplacementAccepted
	if ok, err := st.SaveOrchestratorReplacement(ctx, closed); err == nil || ok {
		t.Fatal("a closed operation was reopened by a delayed write")
	}
}

func TestReplacementStoreStaleSourceAndReservationRelease(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	op := replacementOperation(t, st)
	source, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	source.Metadata.ControllerGeneration = "rotated-generation"
	if err := st.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateOrchestratorReplacement(ctx, op); !errors.Is(err, domain.ErrReplacementSourceChanged) {
		t.Fatalf("stale owner admitted: %v", err)
	}
	op.SourceOwner = source.ControllerOwner()
	accepted, _, err := st.CreateOrchestratorReplacement(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	accepted.Phase = domain.ReplacementCancelled
	if ok, err := st.SaveOrchestratorReplacement(ctx, accepted); err != nil || !ok {
		t.Fatal(ok, err)
	}
	op.ID, op.OperationKey = "55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666"
	if _, created, err := st.CreateOrchestratorReplacement(ctx, op); err != nil || !created {
		t.Fatalf("cancelled operation retained active slot: %t %v", created, err)
	}
}

func TestReplacementStoreCapabilityRotationWithoutGenerationChange(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	op := replacementOperation(t, st)
	source, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	source.Metadata.BrowserCapabilityVerifier = "verifier-after-rotation"
	if err := st.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateOrchestratorReplacement(ctx, op); !errors.Is(err, domain.ErrReplacementSourceChanged) {
		t.Fatalf("rotated capability admitted with stale verifier: %v", err)
	}
}

func TestReplacementStoreKeepsRejectedFactsWithoutBlockingWorkers(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	op := replacementOperation(t, st)
	worker := sampleRecord("replace")
	worker.Harness = domain.HarnessCodex
	if _, err := st.CreateSession(ctx, worker); err != nil {
		t.Fatal(err)
	}
	rejected := op
	rejected.Phase = domain.ReplacementRejected
	rejected.Error = &domain.ReplacementFailure{Code: "ORCHESTRATOR_HANDOFF_TARGET_UNSUPPORTED", Message: "target must use Claude Code"}
	if _, created, err := st.CreateOrchestratorReplacement(ctx, rejected); err != nil || !created {
		t.Fatalf("rejection not durable: %t %v", created, err)
	}
	op.ID, op.OperationKey = "77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888"
	if _, created, err := st.CreateOrchestratorReplacement(ctx, op); err != nil || !created {
		t.Fatalf("rejected fact consumed admission: %t %v", created, err)
	}
	if _, err := st.CreateSession(ctx, worker); err != nil {
		t.Fatalf("active replacement blocked Codex worker creation: %v", err)
	}
	history, found, err := st.GetOrchestratorReplacement(ctx, rejected.ID)
	if err != nil || !found || history.Phase != domain.ReplacementRejected || history.Error.Code != rejected.Error.Code {
		t.Fatalf("rejected result lost: %+v %t %v", history, found, err)
	}
}

func TestReplacementStoreFencesGenericOrchestratorAdmission(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dir)
	op := replacementOperation(t, st)
	// A previously terminated orchestrator must not be restored around the
	// project reservation. Workers remain outside this fence.
	old := sampleRecord("replace")
	old.Kind, old.IsTerminated = domain.KindOrchestrator, true
	old, err := st.CreateSession(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err = st.CreateOrchestratorReplacement(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	otherConnection, err := sqlite.OpenPreMigrated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer otherConnection.Close()
	fresh := sampleRecord("replace")
	fresh.Kind, fresh.Mode = domain.KindOrchestrator, domain.SessionModeChat
	if _, err := otherConnection.CreateSession(ctx, fresh); err == nil {
		t.Error("generic orchestrator creation bypassed replacement reservation")
	}
	old.IsTerminated = false
	if err := otherConnection.UpdateSession(ctx, old); err == nil {
		t.Error("generic restore bypassed replacement reservation")
	}
	worker := sampleRecord("replace")
	if _, err := st.CreateSession(ctx, worker); err != nil {
		t.Fatal(err)
	}
	// Only the durable, hash-bound spawn intent is admitted, after source
	// retirement. Retrying that intent recovers the same seed.
	op.Phase, op.SpawnRequestID, op.SpawnRequestHash = domain.ReplacementStarting, "replacement/"+op.ID, strings.Repeat("c", 64)
	now := time.Now().UTC()
	op.SourceRetiredAt = &now
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	source, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	source.IsTerminated = true
	if err := st.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	fresh.ClientRequestID, fresh.ClientRequestHash = op.SpawnRequestID, op.SpawnRequestHash
	wrong := fresh
	wrong.ClientRequestHash = strings.Repeat("d", 64)
	if _, _, err := st.CreateClientRequestSession(ctx, wrong); err == nil {
		t.Error("mismatched spawn intent admitted")
	}
	created, first, err := st.CreateClientRequestSession(ctx, fresh)
	if err != nil || !first {
		t.Fatalf("reserved successor denied: %t %v", first, err)
	}
	again, first, err := st.CreateClientRequestSession(ctx, fresh)
	if err != nil || first || again.ID != created.ID {
		t.Fatalf("spawn replay lost identity: %t %+v %v", first, again, err)
	}
	current, _, err := st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Phase = domain.ReplacementCompleted
	fixturestore.SeedReplacementTestFacts(t, st, current)
	source.IsTerminated = false
	if err := st.UpdateSession(ctx, source); err == nil {
		t.Error("completed replacement allowed retired source revival")
	}
}

func TestReplacementStoreIncompleteSpawnBindingFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	op.Phase, op.SpawnRequestID = domain.ReplacementStarting, "reserved-key"
	now := time.Now().UTC()
	op.SourceRetiredAt = &now
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	source, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	source.IsTerminated = true
	if err := st.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	fresh := sampleRecord("replace")
	fresh.Kind, fresh.Mode = domain.KindOrchestrator, domain.SessionModeChat
	fresh.ClientRequestID, fresh.ClientRequestHash = op.SpawnRequestID, strings.Repeat("c", 64)
	if _, _, err := st.CreateClientRequestSession(ctx, fresh); err == nil {
		t.Fatal("incomplete operation binding admitted successor through SQL NULL")
	}
}

func TestReplacementRetiredSourceFenceSurvivesCompletedReservation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	source.IsTerminated = true
	if err := st.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op.SourceRetiredAt = &now
	op.Phase = domain.ReplacementCompleted
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	if _, found, err := st.GetActiveOrchestratorReplacement(ctx, op.ProjectID); err != nil || found {
		t.Fatal("completion retained active admission", found, err)
	}
	frozen, found, err := st.GetRetiredOrchestratorReplacementBySource(ctx, source.ID)
	if err != nil || !found || frozen.ID != op.ID || frozen.SourceRetiredAt == nil {
		t.Fatal("completion lost retired source fence", found, err)
	}
	if _, found, err := st.GetRetiredOrchestratorReplacementBySource(ctx, "unrelated-worker"); err != nil || found {
		t.Fatal("retired fence leaked to other session", found, err)
	}
}
