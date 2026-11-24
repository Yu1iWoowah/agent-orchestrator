package review

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
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
	launcher.forceStop.kill = func(process capturedProcess) error {
		if process.pid == 202 {
			table.killed = append(table.killed, process.pid)
			return nil
		}
		return table.kill(process)
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
	launcher.forceStop.freeze = func(process capturedProcess) error {
		if process.pid == 202 {
			return errors.New("freeze denied")
		}
		return table.freeze(process)
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
	if err := table.freeze(capturedProcess{pid: 202}); err != nil {
		t.Fatal(err)
	}
	if err := table.freeze(capturedProcess{pid: 204}); err != nil {
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

func TestReviewerStopProcessFixture(t *testing.T) {
	if os.Getenv("AO_TEST_REVIEWER_STOP_PROCESS") == "1" {
		time.Sleep(time.Minute)
	}
}

func TestReviewerStopKeepsOwnerWithoutOriginalLaunchProof(t *testing.T) {
	for _, tc := range []struct {
		name          string
		restart       bool
		roots         []int
		lostAfterSave bool
		noSealProbe   bool
	}{
		{name: "local pane already gone"},
		{name: "local root exits after capturing and forking", roots: []int{101}, lostAfterSave: true},
		{name: "only one of two roots is visible", roots: []int{101, 102}, noSealProbe: true},
		{name: "boot with pane already gone", restart: true},
		{name: "boot can still read old roots", restart: true, roots: []int{101}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			cfg := RunLifecycleConfig{}
			s.restart(t, &fakeLauncher{handle: "review-mer-1", alive: true}, cfg)
			run := s.trigger(t)
			review := s.review(t)
			account := domain.AgentAccount{ID: "missing-stop-source", Provider: domain.AgentAccountProviderClaude, Home: t.TempDir(), CreatedAt: now}
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
			if pending, err := store.HasReviewerStop(ctx, stopKey(review)); err != nil || pending {
				t.Fatalf("test must start with no stop receipt: %v (%v)", pending, err)
			}

			if tc.restart {
				if err = store.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := sqlite.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				store, s.store = reopened, reopened
				s.advance(time.Second)
				s.restartBeforeBoot(t, &fakeLauncher{}, cfg)
			}
			table := newScriptedTable("202 1 100 0:00 detached-tool")
			if len(tc.roots) != 0 {
				table = newScriptedTable("101 1 100 0:00 pane", "202 101 100 0:00 attached-tool")
			}
			if tc.lostAfterSave {
				table = newScriptedTable("101 1 100 0:00 pane")
			}
			if len(tc.roots) > 1 {
				table = newScriptedTable("101 1 100 0:00 visible-pane", "202 1 100 0:00 detached-tool")
			}
			// The local force-stop path can discover that roots are absent
			// after the earlier liveness probe. The boot path sees the old
			// supervisor gone even when a pane root is still readable.
			rt := &rootedRuntime{fakeRuntime: &fakeRuntime{alive: !tc.restart, supervisedRecord: true}, roots: tc.roots}
			launcher := receiptTestLauncher(root, rt, table, "")
			launcher.forceStop.comparisonScope = func() (string, string) { return "", "" }
			if tc.noSealProbe {
				launcher.forceStop.stopped = nil
			}
			if tc.lostAfterSave {
				reads := 0
				launcher.forceStop.identities = func(ctx context.Context) (map[int]procmem.Identity, error) {
					reads++
					if reads == 2 {
						members, err := store.ListReviewerStopProcesses(ctx, stopKey(review))
						if err != nil || len(members) != 1 || members[0].PID != 101 {
							t.Fatalf("expected root to be committed before it forked and exited: %+v (%v)", members, err)
						}
						table.processes[202] = procmem.Process{PID: 202, PPID: 101}
						table.started[202] = "late-tool"
						delete(table.processes, 101)
						table.processes[202] = procmem.Process{PID: 202, PPID: 1}
						rt.roots = nil
					}
					return table.identities(ctx)
				}
			}
			s.launcher = launcher
			s.advance(DefaultRunLifecycleConfig().CancelTimeout)
			s.pass(t)
			s.pass(t)

			receipts, err := store.ListReviewerStopReceipts(ctx)
			if err != nil || len(receipts) != 1 || receipts[0].ReviewerStopKey != stopKey(review) ||
				receipts[0].State != domain.ReviewerStopUnknown {
				t.Fatalf("expected an unknown receipt for the exact launch: %+v (%v)", receipts, err)
			}
			if _, alive := table.processes[202]; !alive || len(table.killed) != 0 || len(table.frozen) != 0 || rt.destroyed != "" {
				t.Fatal("the unproven launch was stopped or its runtime was destroyed")
			}
			manager := sessionmanager.New(sessionmanager.Deps{Store: store})
			manager.SetReviewerTerminator(accountTestReviewer{s.Engine})
			accounts := agentaccounts.New(store, []ports.AgentAccountProviderCLI{accountTestProvider{}}, root, nil, nil)
			accounts.SetUsageOwners(manager.AccountUsageOwners)
			assertStopOwner(t, store, accounts, account.ID, review.ID)
		})
	}
}

func TestReviewerModeRollbackKeepsOldLaunchStopAuthority(t *testing.T) {
	ctx := t.Context()
	fake := &fakeLauncher{handle: "review-mer-1", agentSessionID: "old-terminal-native"}
	s := newLifecycleEngine(t, fake)
	oldRun := s.trigger(t)
	old := s.review(t)
	now := *s.now
	if moved, err := s.store.TransitionReviewRun(ctx, oldRun.ID, []domain.ReviewRunStatus{domain.ReviewRunRunning},
		domain.ReviewRunCompleted, domain.ReviewRunPatch{Verdict: domain.VerdictApproved, EndedAt: &now}); err != nil || !moved {
		t.Fatalf("complete pass on original pane: %v (%v)", moved, err)
	}
	account := domain.ResolvedAgentAccount{AccountID: "rollback-source-owner", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole}
	if err := s.store.BindReviewerAgentAccount(ctx, old.ID, account, now); err != nil {
		t.Fatal(err)
	}
	fail := errors.New("old pane still alive")
	fake.interfaceMode = domain.ReviewerInterfaceChat
	fake.handle, fake.launchID, fake.agentSessionID = "review-chat:"+old.ID, "", "different-chat-provider"
	fake.destroyErr, fake.destroyErrCall = fail, 1
	s.prs = prAt("sha2")
	if _, err := triggerWithSourceAndMode(ctx, s.Engine, s.worker.ID, old.Harness, domain.AgentConfig{},
		domain.ReviewTriggerManual, domain.ReviewerInterfaceChat); !errors.Is(err, fail) {
		t.Fatalf("expected old pane teardown to fail after replacement spawn: %v", err)
	}
	review := s.review(t)
	if fake.spawnCount != 2 || fake.launchID == old.ReviewerLaunchID || fake.destroyCalls != 2 ||
		review.ReviewerLaunchID != old.ReviewerLaunchID || review.ReviewerHandleID != old.ReviewerHandleID {
		t.Fatalf("replacement did not roll back to the original launch: %+v", review)
	}

	fake.interfaceMode = domain.ReviewerInterfaceTUI
	s.prs = prAt("sha3")
	run := s.trigger(t)
	if fake.spawnCount != 2 || !fake.notified || fake.gotHandle != old.ReviewerHandleID || run.LaunchID != old.ReviewerLaunchID {
		t.Fatalf("expected Notify to reuse original launch: handle=%q run=%+v", fake.gotHandle, run)
	}

	rt := &rootedRuntime{fakeRuntime: &fakeRuntime{alive: true, supervisedRecord: true}, roots: []int{101}}
	table := newScriptedTable("101 1 100 0:00 old-pane", "202 101 100 0:00 provider", "303 1 100 0:00 unrelated")
	launcher := receiptTestLauncher(t.TempDir(), rt, table, "")
	launcher.reviewers = fakeReviewerResolver{reviewer: &fakeCancellableReviewer{}, ok: true}
	s.launcher = launcher
	if _, err := s.Cancel(ctx, s.worker.ID); err != nil {
		t.Fatal(err)
	}
	s.advance(DefaultRunLifecycleConfig().CancelTimeout)
	s.pass(t)
	wantRun(t, s.run(t, run.ID), domain.ReviewRunCancelled, "")
	if rt.destroyed != old.ReviewerHandleID || len(table.killed) != 2 {
		t.Fatalf("original launch was not stopped: destroyed=%q killed=%v", rt.destroyed, table.killed)
	}
	if pending, err := s.store.HasReviewerStop(ctx, stopKey(old)); err != nil || pending {
		t.Fatalf("finished stop remains pending: %v (%v)", pending, err)
	}
	if open, err := s.store.GetOpenReviewerAccountUse(ctx, account.AccountID); err != nil || open != nil {
		t.Fatalf("confirmed stop did not release original account owner: %+v (%v)", open, err)
	}
}

func TestRecoveredChatToTerminalReviewerCanForceStop(t *testing.T) {
	ctx := t.Context()
	s := newLifecycleEngine(t, &fakeLauncher{
		handle: "review-chat:id-1", interfaceMode: domain.ReviewerInterfaceChat, alive: true,
	})
	oldRun := s.trigger(t)
	old := s.review(t)
	if old.InterfaceMode != domain.ReviewerInterfaceChat {
		t.Fatalf("expected a persisted Chat reviewer, got %+v", old)
	}
	if ok, err := s.store.ClaimReviewChatController(ctx, old.ID, "old-provider", "old-generation", *s.now); err != nil || !ok {
		t.Fatalf("seed recoverable Chat controller: %v (%v)", ok, err)
	}

	s.advance(time.Second)
	s.restartBeforeBoot(t, &fakeLauncher{}, RunLifecycleConfig{})
	rt := &rootedRuntime{fakeRuntime: &fakeRuntime{supervisedRecord: true}, roots: []int{101}}
	table := newScriptedTable("101 1 100 0:00 new-pane", "202 101 100 0:00 provider", "303 1 100 0:00 unrelated")
	root := t.TempDir()
	launcher := receiptTestLauncher(root, rt, table, "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reviewer := &struct {
		fakeReviewerWithLaunchSpec
		fakeCancellableReviewer
	}{fakeReviewerWithLaunchSpec: fakeReviewerWithLaunchSpec{spec: ports.ReviewCommandSpec{
		Argv: []string{exe}, AgentSessionID: "new-terminal-native",
	}}}
	launcher.reviewers = fakeReviewerResolver{reviewer: reviewer, ok: true}
	launcher.executable = os.Executable
	s.launcher = launcher
	if err := s.RecoverChatReviewers(ctx); err != nil {
		t.Fatal(err)
	}
	review := s.review(t)
	if !rt.created || review.InterfaceMode != domain.ReviewerInterfaceTUI || review.ReviewerLaunchID == old.ReviewerLaunchID {
		t.Fatalf("startup recovery did not create the new terminal launch: %+v", review)
	}
	wantRun(t, s.run(t, oldRun.ID), domain.ReviewRunFailed, domain.ReviewFailureProcessExited)
	rt.alive = true
	s.pass(t)
	s.prs = prAt("sha2")
	run := s.trigger(t)
	if rt.sentTo != review.ReviewerHandleID || run.LaunchID != review.ReviewerLaunchID {
		t.Fatalf("expected Notify to reuse restored launch: sentTo=%q run=%+v", rt.sentTo, run)
	}
	rt.destroyed = ""
	if _, err := s.Cancel(ctx, s.worker.ID); err != nil {
		t.Fatal(err)
	}
	s.advance(DefaultRunLifecycleConfig().CancelTimeout)
	s.pass(t)
	wantRun(t, s.run(t, run.ID), domain.ReviewRunCancelled, "")
	if rt.destroyed != review.ReviewerHandleID || len(table.killed) != 2 {
		t.Fatalf("the new launch was not stopped: destroyed=%q killed=%v", rt.destroyed, table.killed)
	}
	if pending, err := s.store.HasReviewerStop(ctx, stopKey(review)); err != nil || pending {
		t.Fatalf("finished stop remains pending: %v (%v)", pending, err)
	}
}

type recoveringStopChat struct {
	countingAccountReviewChat
	stops int
}

func (c *recoveringStopChat) RestoreReviewChat(context.Context, ReviewerChatStart) (ports.ChatControllerStarted, error) {
	c.restores++
	return ports.ChatControllerStarted{ProviderConversationID: "old-provider", LiveReconnect: true}, nil
}

func (c *recoveringStopChat) ReviewChatAlive(string) bool { return c.restores != 0 }

func (*recoveringStopChat) ReviewChatStopScope(context.Context, string, string) (*domain.CompactionProcessScope, error) {
	return nil, nil
}

func (c *recoveringStopChat) StopReviewChatGeneration(context.Context, string, string) error {
	c.stops++
	return nil
}

func TestRecoveredLiveChatIsObservedWithoutLocalStop(t *testing.T) {
	ctx := t.Context()
	s := newLifecycleEngine(t, &fakeLauncher{
		handle: "review-chat:id-1", interfaceMode: domain.ReviewerInterfaceChat, alive: true,
	})
	s.trigger(t)
	old := s.review(t)
	if ok, err := s.store.ClaimReviewChatController(ctx, old.ID, "old-provider", "old-generation", *s.now); err != nil || !ok {
		t.Fatalf("seed recoverable Chat controller: %v (%v)", ok, err)
	}
	s.advance(time.Second)
	s.restartBeforeBoot(t, &fakeLauncher{}, RunLifecycleConfig{})
	chat := &recoveringStopChat{}
	launcher := receiptTestLauncher(t.TempDir(), &fakeRuntime{}, newScriptedTable(), "")
	launcher.reviewers = singleReviewerResolver{reviewer: chatReviewAdapter{}}
	launcher.executable = os.Executable
	launcher.chat = chat
	s.launcher = launcher
	if err := s.RecoverChatReviewers(ctx); err != nil {
		t.Fatal(err)
	}
	review := s.review(t)
	if chat.restores != 1 || review.ReviewerLaunchID == old.ReviewerLaunchID || review.ControllerGeneration == "" {
		t.Fatalf("expected an attached older provider under recovered reviewer: %+v", review)
	}
	if written, err := s.beginDurableReviewerStop(ctx, s.store, launcher, review); err != nil || !written {
		t.Fatalf("could not record observer-only stop: %v (%v)", written, err)
	}
	if pending, err := s.store.HasReviewerStop(ctx, stopKey(review)); err != nil || !pending || chat.stops != 0 {
		t.Fatalf("reconnected provider gained local stop authority: pending=%v stops=%d (%v)", pending, chat.stops, err)
	}
}

func TestReviewerStopWithDefaultHooksAndNoRecoveryScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		destroyErr  error
		noSealProbe bool
	}{
		{name: "confirmed in this daemon"},
		{name: "unconfirmed across daemon restart", destroyErr: errors.New("cannot confirm runtime teardown")},
		{name: "native stop without seal probe keeps owner", noSealProbe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			s.restart(t, &fakeLauncher{handle: "review-mer-1", alive: true}, RunLifecycleConfig{})
			run := s.trigger(t)
			review := s.review(t)
			account := domain.ResolvedAgentAccount{
				AccountID: "native-stop-account", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole,
			}
			if err := store.BindReviewerAgentAccount(ctx, review.ID, account, now); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Cancel(ctx, worker.ID); err != nil {
				t.Fatal(err)
			}

			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := aoprocess.CommandContext(ctx, exe, "-test.run=^TestReviewerStopProcessFixture$")
			cmd.Env = append(os.Environ(), "AO_TEST_REVIEWER_STOP_PROCESS=1")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(done)
			}()
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-done
			})

			rt := &rootedRuntime{fakeRuntime: &fakeRuntime{
				alive: true, supervisedRecord: true, destroyErr: tc.destroyErr,
			}, roots: []int{cmd.Process.Pid}}
			// Darwin/Windows default hooks do not supply a cross-restart scope.
			// On Linux, an unavailable canonical root exercises the same branch
			// while still running with the real OS snapshot and signal hooks.
			dataDir := root
			boot, namespace := defaultForceStopHooks().comparisonScope()
			if boot != "" && namespace != "" {
				dataDir = filepath.Join(root, "missing-data-root")
			}
			launcher := &agentLauncher{
				dataDir: dataDir, runtime: rt, reviewers: fakeReviewerResolver{reviewer: &fakeReviewer{}, ok: true},
			}
			if tc.noSealProbe {
				launcher.forceStop = defaultForceStopHooks()
				launcher.forceStop.stopped = nil
			}
			s.launcher = launcher
			s.advance(DefaultRunLifecycleConfig().CancelTimeout)
			s.pass(t)
			select {
			case <-done:
				if cmd.ProcessState.Success() {
					t.Fatal("reviewer exited naturally before the stop")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("default force-stop did not stop the reviewer process")
			}
			if rt.destroyed != review.ReviewerHandleID {
				t.Fatalf("force-stop did not run the original runtime teardown: %q", rt.destroyed)
			}

			if tc.destroyErr == nil && launcher.forceStopHooks().stopped != nil {
				wantRun(t, s.run(t, run.ID), domain.ReviewRunCancelled, "")
				if pending, err := store.HasReviewerStop(ctx, stopKey(review)); err != nil || pending {
					t.Fatalf("confirmed stop left a receipt: %v (%v)", pending, err)
				}
				if owner, err := store.GetOpenReviewerAccountUse(ctx, account.AccountID); err != nil || owner != nil {
					t.Fatalf("confirmed stop kept an account owner: %+v (%v)", owner, err)
				}
				return
			}

			wantRun(t, s.run(t, run.ID), domain.ReviewRunFailed, domain.ReviewFailureCancelUnconfirmed)
			if members, err := store.ListReviewerStopProcesses(ctx, stopKey(review)); err != nil ||
				!slices.ContainsFunc(members, func(p domain.ReviewerStopProcess) bool { return p.PID == cmd.Process.Pid && p.Start != "" }) {
				t.Fatalf("stop did not persist the original process: %+v (%v)", members, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := sqlite.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			store, s.store = reopened, reopened
			nextRuntime := &rootedRuntime{fakeRuntime: &fakeRuntime{}}
			s.restartBeforeBoot(t, &fakeLauncher{}, RunLifecycleConfig{})
			s.launcher = &agentLauncher{dataDir: dataDir, runtime: nextRuntime}
			s.pass(t)
			s.pass(t)
			if nextRuntime.destroyed != "" {
				t.Fatal("recovery attempted another native stop")
			}
			if owner, err := store.GetOpenReviewerAccountUse(ctx, account.AccountID); err != nil || owner == nil {
				t.Fatalf("recovery released the original account: %+v (%v)", owner, err)
			}
			if owners, err := store.ListAgentAccountUsageOwners(ctx, account.AccountID); err != nil || len(owners) != 1 || !owners[0].Blocking {
				t.Fatalf("recovery lost the effective account owner: %+v (%v)", owners, err)
			}
		})
	}
}

type lateReceiptRuntime struct {
	*rootedRuntime
	entered chan struct{}
	resume  chan struct{}
}

func (r *lateReceiptRuntime) Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	close(r.entered)
	select {
	case <-r.resume:
		return r.rootedRuntime.Create(ctx, cfg)
	case <-ctx.Done():
		return ports.RuntimeHandle{}, ctx.Err()
	}
}

func TestLateSpawnFailureRetainsStopOwnerAfterReopen(t *testing.T) {
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
	cfg := RunLifecycleConfig{LaunchTimeout: time.Second}
	s.restart(t, &fakeLauncher{}, cfg)
	rt := &lateReceiptRuntime{
		rootedRuntime: &rootedRuntime{fakeRuntime: &fakeRuntime{}, roots: []int{101}},
		entered:       make(chan struct{}),
		resume:        make(chan struct{}),
	}
	table := newScriptedTable("101 1 100 0:00 pane", "202 101 100 0:00 survivor", "303 1 100 0:00 other")
	launcher := receiptTestLauncher(root, rt, table, "boot-1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher.reviewers = fakeReviewerResolver{reviewer: &fakeReviewerForPreflight{Argv: []string{executable}}, ok: true}
	launcher.executable = os.Executable
	launcher.forceStop.kill = func(process capturedProcess) error {
		if process.pid == 202 {
			return nil
		}
		return table.kill(process)
	}
	s.launcher = launcher
	launchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.TriggerWithOptions(launchCtx, worker.ID, TriggerOptions{})
		done <- err
	}()
	select {
	case <-rt.entered:
	case early := <-done:
		t.Fatalf("trigger returned before runtime Create: %v", early)
	case <-time.After(20 * time.Second):
		t.Fatal("trigger did not reach runtime Create")
	}
	review := s.review(t)
	account := domain.ResolvedAgentAccount{
		AccountID: "late-reviewer-account", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole,
	}
	if err := store.BindReviewerAgentAccount(ctx, review.ID, account, now); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListActiveReviewRunsBySession(ctx, worker.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("launching runs: %+v (%v)", runs, err)
	}
	s.advance(2 * time.Second)
	s.pass(t)
	wantRun(t, s.run(t, runs[0].ID), domain.ReviewRunFailed, domain.ReviewFailureNeverStarted)
	close(rt.resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("trigger did not finish after runtime Create resumed")
	}
	review = s.review(t)
	if rt.destroyed == "" || review.ReviewerHandleID != rt.destroyed {
		t.Fatalf("pane teardown cleared the unconfirmed original handle: review=%+v pane=%q", review, rt.destroyed)
	}
	if pending, err := store.HasReviewerStop(ctx, stopKey(review)); err != nil || !pending {
		t.Fatalf("late-spawn cleanup did not journal the survivor: %v (%v)", pending, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	store, s.store = reopened, reopened
	s.restartBeforeBoot(t, &fakeLauncher{}, cfg)
	newRuntime := &rootedRuntime{fakeRuntime: &fakeRuntime{}}
	s.launcher = receiptTestLauncher(root, newRuntime, table, "boot-1")
	s.pass(t)
	if err := s.ReconcileReviewers(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	if owners, err := store.ListAgentAccountUsageOwners(ctx, account.AccountID); err != nil || len(owners) != 1 || !owners[0].Blocking {
		t.Fatalf("reopened owner list released the late-spawn survivor: %+v (%v)", owners, err)
	}
	if owner, err := store.GetOpenReviewerAccountUse(ctx, account.AccountID); err != nil || owner == nil {
		t.Fatalf("reopened mutation guard released the late-spawn survivor: %+v (%v)", owner, err)
	}
	if newRuntime.destroyed != "" {
		t.Fatal("receipt recovery destroyed a pane")
	}
	wantRun(t, s.run(t, runs[0].ID), domain.ReviewRunFailed, domain.ReviewFailureNeverStarted)
}

func TestReviewerStopRetriesInitiallyUnreadableProcessEvidence(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			ctx := t.Context()
			s := newLifecycleEngine(t, &fakeLauncher{handle: "review-mer-1"})
			s.trigger(t)
			review := s.review(t)
			root := t.TempDir()
			table := newScriptedTable("101 1 100 0:00 pane", "202 101 100 0:00 provider", "303 1 100 0:00 other")
			rt := &rootedRuntime{fakeRuntime: &fakeRuntime{}, roots: []int{101}}
			launcher := receiptTestLauncher(root, rt, table, "boot-1")
			launcher.forceStop.identities = func(context.Context) (map[int]procmem.Identity, error) {
				return nil, errors.New("initial process snapshot unavailable")
			}
			s.launcher = launcher
			if written, err := s.beginDurableReviewerStop(ctx, s.store, launcher, review); err != nil || !written {
				t.Fatalf("record retryable stop: written=%v err=%v", written, err)
			}
			receipts, err := s.store.ListReviewerStopReceipts(ctx)
			if err != nil || len(receipts) != 1 || receipts[0].State != domain.ReviewerStopUnknown || receipts[0].Reason != "reviewer_roots_unavailable" {
				t.Fatalf("first sample did not retain the original scope: %+v (%v)", receipts, err)
			}
			if restart {
				s.restartBeforeBoot(t, &fakeLauncher{}, RunLifecycleConfig{})
				launcher = receiptTestLauncher(root, rt, table, "boot-1")
				s.launcher = launcher
			}
			launcher.forceStop.identities = table.identities
			s.pass(t)
			pending, err := s.store.HasReviewerStop(ctx, stopKey(review))
			if err != nil || pending != restart {
				t.Fatalf("pending stop after usable evidence: %v (%v)", pending, err)
			}
			if !restart {
				if len(table.killed) == 0 || rt.destroyed == "" {
					t.Fatal("same-daemon recovery did not complete the native stop")
				}
				return
			}
			receipts, err = s.store.ListReviewerStopReceipts(ctx)
			if err != nil || len(receipts) != 1 || receipts[0].State != domain.ReviewerStopCapturing {
				t.Fatalf("restored stop did not persist the available evidence: %+v (%v)", receipts, err)
			}
			if len(table.frozen) != 0 || len(table.killed) != 0 || rt.destroyed != "" {
				t.Fatal("a restored receipt tried to stop a process")
			}
		})
	}
}
