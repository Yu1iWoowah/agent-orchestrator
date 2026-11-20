package review

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/procmem"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func stopTestExec(t *testing.T, root, query string, args ...any) {
	t.Helper()
	u := url.URL{Path: filepath.Join(root, "ao.db")}
	db, err := sql.Open("sqlite", "file:"+u.EscapedPath()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func receiptTestLauncher(root string, rt reviewerRuntime, table *scriptedTable, boot string) *agentLauncher {
	launcher := forceStopLauncher(rt, table)
	launcher.dataDir = root
	launcher.reviewers = fakeReviewerResolver{reviewer: &fakeReviewer{}, ok: true}
	launcher.forceStop.comparisonScope = func() (string, string) { return boot, "pid-namespace" }
	launcher.forceStop.freeze = table.freeze
	launcher.forceStop.stopped = func(pid int, _ string) bool { return slices.Contains(table.frozen, pid) }
	return launcher
}

func assertStopOwner(t *testing.T, store *sqlite.Store, accounts *agentaccounts.Service, accountID, reviewID string) {
	t.Helper()
	ctx := t.Context()
	owners, err := accounts.UsageOwners(ctx, accountID)
	if err != nil || len(owners) != 1 || owners[0].ReviewID != reviewID || !owners[0].Blocking {
		t.Fatalf("effective blocking owner: %+v (%v)", owners, err)
	}
	open, err := store.GetOpenReviewerAccountUse(ctx, accountID)
	if err != nil || open == nil || open.ReviewID != reviewID {
		t.Fatalf("open reviewer owner: %+v (%v)", open, err)
	}
	if runtime.GOOS == "linux" {
		if err := accounts.Remove(ctx, accountID); !errors.Is(err, ports.ErrAgentAccountInUse) {
			t.Fatalf("remove before stop proof: %v", err)
		}
	}
}

func TestUnconfirmedReviewerStopOwnsAccountAfterSQLiteReopen(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	store, err := sqlite.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	worker := liveWorker()
	seedReviewWorker(t, store, worker)
	now := time.Now().UTC()
	s := lifecycleEngine{store: store, worker: worker, now: &now, ids: new(int)}
	fake := &fakeLauncher{handle: "review-mer-1", alive: true}
	s.restart(t, fake, RunLifecycleConfig{})
	run := s.trigger(t)
	review := s.review(t)
	account := domain.AgentAccount{ID: "stop-owner", Provider: domain.AgentAccountProviderClaude, Home: t.TempDir(), CreatedAt: now}
	if err := store.CreateAgentAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := store.BindReviewerAgentAccount(ctx, review.ID, domain.ResolvedAgentAccount{
		AccountID: account.ID, Provider: account.Provider, Source: domain.AgentAccountSourceRole,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	wantRun(t, s.run(t, run.ID), domain.ReviewRunCancelling, "")

	table := newScriptedTable("101 1 100 0:00 pane", "202 101 100 0:00 provider", "303 1 100 0:00 unrelated")
	runtime := &rootedRuntime{fakeRuntime: &fakeRuntime{alive: true, supervisedRecord: true}, roots: []int{101}}
	launcher := receiptTestLauncher(root, runtime, table, "boot-1")
	launcher.forceStop.kill = func(pid int) error {
		if pid == 202 {
			table.killed = append(table.killed, pid)
			return nil
		}
		return table.kill(pid)
	}
	stopTestExec(t, root, `CREATE TRIGGER injected_stop_insert BEFORE INSERT ON reviewer_stop_accounts
		BEGIN SELECT RAISE(ABORT, 'injected stop insert'); END`)
	s.launcher = launcher
	s.advance(DefaultRunLifecycleConfig().CancelTimeout)
	if err := s.reaper.Tick(ctx); err == nil || !strings.Contains(err.Error(), "injected stop insert") {
		t.Fatalf("initial receipt error: %v", err)
	}
	if pending, err := store.HasReviewerStop(ctx, stopKey(review)); err != nil || pending {
		t.Fatalf("failed initial write left a partial receipt: %v (%v)", pending, err)
	}
	wantRun(t, s.run(t, run.ID), domain.ReviewRunCancelling, "")
	if len(table.frozen) != 0 || len(table.killed) != 0 || runtime.destroyed != "" {
		t.Fatal("stop affected the runtime before the receipt committed")
	}
	stopTestExec(t, root, "DROP TRIGGER injected_stop_insert")
	s.pass(t)
	wantRun(t, s.run(t, run.ID), domain.ReviewRunFailed, domain.ReviewFailureCancelUnconfirmed)
	if runtime.destroyed != review.ReviewerHandleID {
		t.Fatalf("reviewer pane was not removed: %q", runtime.destroyed)
	}
	if _, err := s.TriggerWithOptions(ctx, worker.ID, TriggerOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("trigger reused the receipt's launch: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	store = reopened
	s.store = store

	newRuntime := &rootedRuntime{fakeRuntime: &fakeRuntime{}}
	restarted := receiptTestLauncher(root, newRuntime, table, "boot-1")
	beforeFreeze, beforeKill := len(table.frozen), len(table.killed)
	s.restartBeforeBoot(t, &fakeLauncher{}, RunLifecycleConfig{})
	s.launcher = restarted
	manager := sessionmanager.New(sessionmanager.Deps{Store: store})
	manager.SetReviewerTerminator(accountTestReviewer{s.Engine})
	accounts := agentaccounts.New(store, []ports.AgentAccountProviderCLI{accountTestProvider{}}, root, nil, nil)
	accounts.SetUsageOwners(manager.AccountUsageOwners)
	assertStopOwner(t, store, accounts, account.ID, review.ID)
	s.pass(t)
	s.pass(t)
	assertStopOwner(t, store, accounts, account.ID, review.ID)

	for _, origin := range []struct {
		name   string
		bootID string
		root   string
		reason string
	}{
		{"same path and machine-id copy from another boot", "boot-2", root, "boot_changed_origin_unverified"},
		{"different root", "boot-1", t.TempDir(), "data_root_mismatch"},
	} {
		t.Run(origin.name, func(t *testing.T) {
			// A machine-id match carries no authority and is not a receipt input.
			probe := receiptTestLauncher(origin.root, newRuntime, table, origin.bootID)
			probe.forceStop.identities = func(context.Context) (map[int]procmem.Identity, error) {
				t.Fatal("attempted a PID probe outside the stored scope")
				return nil, nil
			}
			s.launcher = probe
			s.pass(t)
			receipts, err := store.ListReviewerStopReceipts(ctx)
			if err != nil || len(receipts) != 1 || receipts[0].Reason != origin.reason {
				t.Fatalf("retained receipt: %+v (%v)", receipts, err)
			}
			assertStopOwner(t, store, accounts, account.ID, review.ID)
		})
	}
	s.launcher = restarted

	// Reuse the survivor PID. A comparison in the original scope can prove
	// that process gone, and must leave the replacement alone.
	table.started[202] = "new-start"
	stopTestExec(t, root, `CREATE TRIGGER injected_stop_delete BEFORE DELETE ON reviewer_stop_receipts
		BEGIN SELECT RAISE(ABORT, 'injected stop delete'); END`)
	if err := s.reaper.Tick(ctx); err == nil || !strings.Contains(err.Error(), "injected stop delete") {
		t.Fatalf("settlement failure: %v", err)
	}
	if s.review(t).ReviewerHandleID != review.ReviewerHandleID {
		t.Fatal("failed settlement made a partial runtime update")
	}
	assertStopOwner(t, store, accounts, account.ID, review.ID)
	stopTestExec(t, root, "DROP TRIGGER injected_stop_delete")
	s.pass(t)
	if got, err := store.GetOpenReviewerAccountUse(ctx, account.ID); err != nil || got != nil {
		t.Fatalf("account still blocked after proof: %+v (%v)", got, err)
	}
	if owners, err := accounts.UsageOwners(ctx, account.ID); err != nil || len(owners) != 1 || owners[0].Blocking {
		t.Fatalf("effective owner after proof: %+v (%v)", owners, err)
	}
	if len(table.frozen) != beforeFreeze || len(table.killed) != beforeKill || newRuntime.destroyed != "" {
		t.Fatalf("recovered path stopped a process or runtime: frozen=%v killed=%v runtime=%q", table.frozen, table.killed, newRuntime.destroyed)
	}
}

func TestReviewerStopSealsOnlyAfterFinalLineage(t *testing.T) {
	ctx := t.Context()
	s := newLifecycleEngine(t, &fakeLauncher{handle: "review-mer-1"})
	s.trigger(t)
	review := s.review(t)
	root := t.TempDir()
	table := newScriptedTable("101 1 100 0:00 pane", "202 101 100 0:00 provider", "303 1 100 0:00 other")
	rt := &rootedRuntime{fakeRuntime: &fakeRuntime{}, roots: []int{101}}
	launcher := receiptTestLauncher(root, rt, table, "boot-1")
	s.launcher = launcher
	launcher.forceStop.freeze = func(pid int) error {
		if pid == 202 {
			return errors.New("freeze denied")
		}
		return table.freeze(pid)
	}
	if written, err := s.beginDurableReviewerStop(ctx, s.store, launcher, review); !written || err == nil {
		t.Fatalf("failed freeze must follow the receipt commit: written=%v err=%v", written, err)
	}
	receipts, err := s.store.ListReviewerStopReceipts(ctx)
	if err != nil || len(receipts) != 1 || receipts[0].State != domain.ReviewerStopCapturing || receipts[0].Reason != "freeze_unconfirmed" {
		t.Fatalf("unsealed receipt after freeze failure: %+v (%v)", receipts, err)
	}
	if len(table.killed) != 0 || rt.destroyed != "" {
		t.Fatal("an unsealed receipt allowed runtime teardown")
	}
	if err := s.TeardownReviewerTerminal(ctx, s.worker.ID); !errors.Is(err, domain.ErrReviewerStopPending) {
		t.Fatalf("teardown erased unsealed evidence: %v", err)
	}

	// The still-running child created a grandchild after the initial capture.
	// A restarted daemon can see it while the original root is still present,
	// but cannot seal or stop it.
	table.processes[204] = procmem.Process{PID: 204, PPID: 202}
	table.started[204] = "late-start"
	s.restartBeforeBoot(t, &fakeLauncher{}, RunLifecycleConfig{})
	s.launcher = receiptTestLauncher(root, rt, table, "boot-1")
	s.pass(t)
	processes, err := s.store.ListReviewerStopProcesses(ctx, stopKey(review))
	if err != nil || len(processes) != 3 || !slices.ContainsFunc(processes, func(p domain.ReviewerStopProcess) bool { return p.PID == 204 }) {
		t.Fatalf("late lineage after restart: %+v (%v)", processes, err)
	}
	if len(table.frozen) != 1 || len(table.killed) != 0 || rt.destroyed != "" {
		t.Fatalf("recovery tried to stop reviewer: frozen=%v killed=%v pane=%q", table.frozen, table.killed, rt.destroyed)
	}

	// The original runtime remains observable, and all members later enter a
	// stopped state independently of the new daemon.
	if err := table.freeze(202); err != nil {
		t.Fatal(err)
	}
	if err := table.freeze(204); err != nil {
		t.Fatal(err)
	}
	s.pass(t)
	receipts, err = s.store.ListReviewerStopReceipts(ctx)
	if err != nil || len(receipts) != 1 || receipts[0].State != domain.ReviewerStopSealed {
		t.Fatalf("complete late lineage did not seal: %+v (%v)", receipts, err)
	}
	if len(table.killed) != 0 || rt.destroyed != "" {
		t.Fatal("observation sent a stop after sealing")
	}
}

func TestRecoveredChatStopUsesOldScopeAfterNewGeneration(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	store, err := sqlite.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	worker := liveWorker()
	seedReviewWorker(t, store, worker)
	now := time.Now().UTC()
	s := lifecycleEngine{store: store, worker: worker, now: &now, ids: new(int)}
	s.restart(t, &fakeLauncher{handle: "review-mer-1"}, RunLifecycleConfig{})
	s.trigger(t)
	old := s.review(t)
	old.InterfaceMode = domain.ReviewerInterfaceChat
	old.ControllerGeneration = "old-controller"
	if err := s.store.UpsertReview(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := store.BindReviewerAgentAccount(ctx, old.ID, domain.ResolvedAgentAccount{
		AccountID: "old-account", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole,
	}, now); err != nil {
		t.Fatal(err)
	}
	scope := &domain.CompactionProcessScope{
		HostPID: 101, HostBirth: "old-host", ProviderPGID: 202, LeaderPID: 202, LeaderBirth: "old-provider",
		BootID: "boot-1", PIDNamespace: "pid-namespace",
	}
	receipt := domain.ReviewerStopReceipt{
		ReviewerStopKey: stopKey(old), SessionID: old.SessionID, HandleID: old.ReviewerHandleID,
		InterfaceMode: old.InterfaceMode, ControllerGeneration: old.ControllerGeneration,
		DataRoot: root, BootID: scope.BootID, PIDNamespace: scope.PIDNamespace,
		State: domain.ReviewerStopCapturing, ChatScope: scope,
	}
	if err := s.store.BeginReviewerStop(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	gone := false
	calls := 0
	launcher := receiptTestLauncher(root, &fakeRuntime{alive: true, supervisedRecord: true}, newScriptedTable(), "boot-1")
	launcher.forceStop.chatEnded = func(_ context.Context, current *domain.CompactionProcessScope) (bool, string) {
		calls++
		if *current != *scope {
			t.Fatalf("observed different scope: %+v", current)
		}
		return gone, ""
	}
	s.restartBeforeBoot(t, &fakeLauncher{}, RunLifecycleConfig{})
	s.launcher = launcher
	if err := s.RecoverChatReviewers(ctx); err != nil {
		t.Fatalf("recovery attempted to attach missing original controller: %v", err)
	}
	s.pass(t)
	if pending, err := s.store.HasReviewerStop(ctx, stopKey(old)); err != nil || !pending || calls != 1 {
		t.Fatalf("old provider scope released with no controller: pending=%v probes=%d (%v)", pending, calls, err)
	}

	newReview := old
	newReview.ReviewerLaunchID, newReview.ControllerGeneration = "new-launch", "new-controller"
	newReview.ReviewerActivityState = domain.ActivityActive
	if err := s.store.UpsertReview(ctx, newReview); err != nil {
		t.Fatal(err)
	}
	// Model the replacement's current binding under the same alias while
	// leaving the old account snapshot in the original launch's receipt.
	stopTestExec(t, root, "UPDATE reviewer_agent_accounts SET account_id='new-account' WHERE review_id=?", old.ID)
	if owner, err := store.GetOpenReviewerAccountUse(ctx, "old-account"); err != nil || owner == nil {
		t.Fatalf("lost original captured account after replacement: %+v (%v)", owner, err)
	}
	gone = true
	s.pass(t)
	if pending, err := s.store.HasReviewerStop(ctx, stopKey(old)); err != nil || pending {
		t.Fatalf("confirmed gone scope is still held: %v (%v)", pending, err)
	}
	current := s.review(t)
	if current.ReviewerLaunchID != newReview.ReviewerLaunchID || current.ControllerGeneration != newReview.ControllerGeneration || current.ReviewerHandleID != newReview.ReviewerHandleID {
		t.Fatalf("old scope cleared the new reviewer: %+v", current)
	}
	if owner, err := store.GetOpenReviewerAccountUse(ctx, "old-account"); err != nil || owner != nil {
		t.Fatalf("old account retained after settlement: %+v (%v)", owner, err)
	}
	if owner, err := store.GetOpenReviewerAccountUse(ctx, "new-account"); err != nil || owner == nil || owner.ReviewID != newReview.ID {
		t.Fatalf("new account released by the old receipt: %+v (%v)", owner, err)
	}
}
