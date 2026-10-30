package sessionmanager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tmuxruntime "github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/tmux"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func privateRecoveryTmux(t *testing.T) (*tmuxruntime.Runtime, string) {
	t.Helper()
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("private runtime regression requires tmux")
	}
	// Keep Unix socket paths short even when testing under a private TMPDIR.
	root, err := os.MkdirTemp(os.TempDir(), "r-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMUX_TMPDIR", root)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	socket := fmt.Sprintf("u3-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, binary, "-L", socket, "kill-server").Run()
	})
	return tmuxruntime.New(tmuxruntime.Options{Binary: binary, LegacyBinary: binary, SocketName: socket, Timeout: 5 * time.Second, ReapGrace: 10 * time.Millisecond}), root
}

func TestSQLiteCommittedSuccessorKeepsItsTmuxWriter(t *testing.T) {
	for _, tc := range []struct {
		name               string
		phase, tail        domain.SessionInterfaceTransitionPhase
		legacy, terminated bool
	}{
		{name: "target_starting", phase: domain.SessionInterfaceTransitionTargetStarting},
		{name: "activating", phase: domain.SessionInterfaceTransitionActivating},
		{name: "recovery_required", phase: domain.SessionInterfaceTransitionRecovery},
		{name: "completed then failed", phase: domain.SessionInterfaceTransitionCompleted, tail: domain.SessionInterfaceTransitionFailed},
		{name: "completed then cancelled", phase: domain.SessionInterfaceTransitionCompleted, tail: domain.SessionInterfaceTransitionCancelled},
		{name: "committed then failed", phase: domain.SessionInterfaceTransitionRecovery, tail: domain.SessionInterfaceTransitionFailed},
		{name: "legacy resumed", legacy: true},
		{name: "legacy then cancelled", legacy: true, tail: domain.SessionInterfaceTransitionCancelled},
		{name: "started then resumed"},
		{name: "terminated legacy", legacy: true, terminated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			runtime, root := privateRecoveryTmux(t)
			db := sqlitetest.MustOpenAt(t, filepath.Join(root, "db"))
			now := time.Now().UTC()
			if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "recovery", Path: root, RegisteredAt: now}); err != nil {
				t.Fatal(err)
			}
			rec, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "recovery", Harness: domain.HarnessClaudeCode, Kind: domain.KindWorker, Mode: domain.SessionModeTUI, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			handle, err := runtime.Create(ctx, ports.RuntimeConfig{SessionID: rec.ID, WorkspacePath: root, Argv: []string{"/bin/sleep", "600"}})
			if err != nil {
				t.Fatal(err)
			}
			rec.Metadata.RuntimeHandleID, rec.Metadata.RuntimeLaunchID = handle.ID, "new-generation"
			if tc.phase != "" {
				// Force the SQL successor proof, independently of the resumed
				// generation proof exercised by the no-successor cases below.
				rec.Metadata.RuntimeLaunchID = ""
			}
			rec.IsTerminated = tc.terminated
			if err := db.UpdateSession(ctx, rec); err != nil {
				t.Fatal(err)
			}
			bindings := []string{"acct-a"}
			if tc.phase != "" {
				bindings = append(bindings, "acct-b", "acct-c")
			}
			for i, account := range bindings {
				if err := db.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderClaude, account, now.Add(time.Duration(i)*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			for i, id := range []string{"old-receipt", "new-committed"} {
				if i == 1 && tc.phase == "" {
					continue
				}
				at := now.Add(time.Duration(i) * time.Second)
				accounts := []string{"acct-a", "acct-b", "acct-c"}
				move := domain.SessionInterfaceTransition{ID: id, SessionID: rec.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, Policy: domain.SessionInterfaceTransitionDrain, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, SourceAccountID: accounts[i], TargetAccountID: accounts[i+1], Phase: domain.SessionInterfaceTransitionTargetStarting, CreatedAt: at, UpdatedAt: at}
				if _, created, err := db.CreateSessionInterfaceTransition(ctx, move); err != nil || !created {
					t.Fatalf("create receipt: created=%t err=%v", created, err)
				}
				generation := "old-generation"
				if i == 1 {
					generation = "new-generation"
				}
				for _, state := range []domain.AccountTransitionRuntimeState{domain.AccountTransitionRuntimeReserved, domain.AccountTransitionRuntimeStarted} {
					h := ""
					if state == domain.AccountTransitionRuntimeStarted {
						h = handle.ID // tmux reuses this physical handle across generations.
					}
					if changed, err := db.RecordAccountTransitionRuntime(ctx, id, state, generation, h, at); err != nil || !changed {
						t.Fatalf("record %s: changed=%t err=%v", state, changed, err)
					}
				}
				targetPhase := domain.SessionInterfaceTransitionRecovery
				if i == 1 {
					targetPhase = tc.phase
				}
				if targetPhase != move.Phase {
					if changed, err := db.AdvanceSessionInterfaceTransition(ctx, id, move.Phase, targetPhase, "", "DAEMON_RESTARTED", "", at); err != nil || !changed {
						t.Fatalf("advance: changed=%t err=%v", changed, err)
					}
				}
			}

			if tc.legacy {
				raw, err := sql.Open("sqlite", "file:"+filepath.Join(root, "db", "ao.db"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = raw.ExecContext(ctx, "UPDATE session_interface_transitions SET target_runtime_state='',target_runtime_launch_id='',target_runtime_handle_id='' WHERE id='old-receipt'")
				_ = raw.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.tail != "" {
				_, created, err := db.CreateSessionInterfaceTransition(ctx, domain.SessionInterfaceTransition{ID: "later-unsuccessful", SessionID: rec.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, Policy: domain.SessionInterfaceTransitionDrain, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, SourceAccountID: bindings[len(bindings)-1], TargetAccountID: "acct-d", Phase: tc.tail, CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second)})
				if err != nil || !created {
					t.Fatalf("create tail: %t %v", created, err)
				}
			}
			manager, _, _, _, accounts, _ := newAccountTransitionManager(t, domain.SessionModeTUI)
			manager.store, manager.runtime = db, runtime
			manager.SetAgentAccounts(sqliteRecoveryAccounts{transitionAccounts: accounts, db: db})
			if _, err := manager.recoverInterruptedInterfaceTransitions(ctx); err != nil {
				t.Fatal(err)
			}
			alive, err := runtime.IsAlive(ctx, handle)
			if alive == tc.terminated || err != nil && !errors.Is(err, ports.ErrRuntimeUnavailable) {
				t.Fatalf("recovery changed the wrong writer: alive=%t terminated=%t err=%v", alive, tc.terminated, err)
			}
			old, found, err := db.GetSessionInterfaceTransition(ctx, "old-receipt")
			if err != nil || !found || old.TargetRuntimeState != domain.AccountTransitionRuntimeStopped {
				t.Fatalf("superseded receipt did not retire: %+v found=%t err=%v", old, found, err)
			}
			bound, _, err := db.GetSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderClaude)
			if err != nil || bound != bindings[len(bindings)-1] || manager.SessionMutationInProgress(rec.ID) {
				t.Fatalf("committed binding/fence changed: bound=%s err=%v", bound, err)
			}
			owners, err := db.ListAgentAccountUsageOwners(ctx, "acct-b")
			if err != nil {
				t.Fatal(err)
			}
			for _, owner := range owners {
				if owner.Blocking {
					t.Fatalf("retired target still blocks lifecycle: %+v", owner)
				}
			}
		})
	}
}

func TestUnknownAccountReceiptStopsActualTmuxNamespace(t *testing.T) {
	for name, id := range map[string]domain.SessionID{
		"short":      "recovery-1",
		"long":       domain.SessionID(strings.Repeat("recovery-project-", 5) + "1"),
		"normalized": "recovery.project:1",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			runtime, root := privateRecoveryTmux(t)
			handle, err := runtime.Create(ctx, ports.RuntimeConfig{SessionID: id, WorkspacePath: root, Argv: []string{"/bin/sleep", "600"}})
			if err != nil {
				t.Fatal(err)
			}
			manager, st, _, _, accounts, _ := newAccountTransitionManager(t, domain.SessionModeTUI)
			rec := st.sessions["session-1"]
			delete(st.sessions, rec.ID)
			rec.ID, rec.Metadata.RuntimeHandleID = id, handle.ID
			st.sessions[id] = rec
			accounts.bound[id] = "device-claude"
			manager.runtime = runtime
			st.transitions["unknown-receipt"] = domain.SessionInterfaceTransition{ID: "unknown-receipt", SessionID: id, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, SourceAccountID: "device-claude", TargetAccountID: "acct-b", Phase: domain.SessionInterfaceTransitionTargetStarting, CreatedAt: time.Now()}
			if _, err := manager.recoverInterruptedInterfaceTransitions(ctx); err != nil {
				t.Fatal(err)
			}
			alive, err := runtime.IsAlive(ctx, handle)
			if err != nil && !errors.Is(err, ports.ErrRuntimeUnavailable) {
				t.Fatal(err)
			}
			if alive {
				t.Errorf("namespace recovery left writer alive: raw=%s handle=%s", id, handle.ID)
			}
			move, _, _ := st.GetSessionInterfaceTransition(ctx, "unknown-receipt")
			if move.TargetRuntimeState != domain.AccountTransitionRuntimeStopped || manager.SessionMutationInProgress(id) {
				t.Errorf("confirmed namespace shutdown did not converge: %+v", move)
			}
		})
	}
}
