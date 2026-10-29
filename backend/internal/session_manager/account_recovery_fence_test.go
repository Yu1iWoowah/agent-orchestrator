package sessionmanager

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type orderedRecoveryStore struct {
	*transitionStore
	reverse bool
}

func (s orderedRecoveryStore) ListUnsettledAccountTransitions(ctx context.Context) ([]domain.SessionInterfaceTransition, error) {
	rows, err := s.transitionStore.ListUnsettledAccountTransitions(ctx)
	sort.Slice(rows, func(i, j int) bool {
		if s.reverse {
			return rows[i].ID > rows[j].ID
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, err
}

type blockedRecoveryRuntime struct {
	*transitionRuntime
	blocked   map[string]bool
	onDestroy func()
}

func (r blockedRecoveryRuntime) Destroy(ctx context.Context, h ports.RuntimeHandle) error {
	if r.onDestroy != nil {
		r.onDestroy()
	}
	if r.blocked[h.ID] {
		return errors.New("private-stop-canary")
	}
	return r.transitionRuntime.Destroy(ctx, h)
}

func TestRecoveryFenceCoversEveryReceiptRegardlessOfOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked first", true: "settled first"}[reverse], func(t *testing.T) {
			ctx := context.Background()
			m, st, rt, _, _, _ := newAccountTransitionManager(t, domain.SessionModeTUI)
			m.store = orderedRecoveryStore{transitionStore: st, reverse: reverse}
			rec := st.sessions["session-1"]
			rec.Metadata.RuntimeLaunchID = ""
			st.sessions[rec.ID] = rec
			now := time.Now()
			rt.aliveByHandle = make(map[string]bool)
			for i, id := range []string{"a-blocked", "b-settled"} {
				st.transitions[id] = domain.SessionInterfaceTransition{ID: id, SessionID: rec.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, SourceAccountID: "device-claude", TargetAccountID: id, Phase: domain.SessionInterfaceTransitionRecovery, TargetRuntimeState: domain.AccountTransitionRuntimeStarted, TargetRuntimeLaunchID: id, TargetRuntimeHandleID: id, CreatedAt: now.Add(time.Duration(i) * time.Second)}
				rt.aliveByHandle[id] = true
			}
			blocked := map[string]bool{"a-blocked": true}
			m.runtime = blockedRecoveryRuntime{transitionRuntime: rt, blocked: blocked, onDestroy: func() {
				if release, ok := m.AcquireSessionInput(rec.ID); ok {
					release()
					t.Error("one receipt reopened input while another was still recovering")
				}
			}}
			if _, err := m.recoverInterruptedInterfaceTransitions(ctx); err != nil {
				t.Fatal(err)
			}
			pending, _, _ := st.GetSessionInterfaceTransition(ctx, "a-blocked")
			settled, _, _ := st.GetSessionInterfaceTransition(ctx, "b-settled")
			if !m.SessionMutationInProgress(rec.ID) || pending.TargetRuntimeState != domain.AccountTransitionRuntimeStarted || settled.TargetRuntimeState != domain.AccountTransitionRuntimeStopped || pending.ErrorCode != "TARGET_STOP_UNCONFIRMED" {
				t.Fatalf("receipt fences overwrote each other: pending=%+v settled=%+v", pending, settled)
			}
			blocked["a-blocked"] = false
			if _, err := m.recoverInterruptedInterfaceTransitions(ctx); err != nil {
				t.Fatal(err)
			}
			if m.SessionMutationInProgress(rec.ID) {
				t.Fatal("confirmed retry did not release the session fence")
			}
		})
	}
}

func TestUnconfirmedHistoricalRecoveryIsDurableAndVisible(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := sqlitetest.MustOpenAt(t, filepath.Join(root, "db"))
	now := time.Now().UTC()
	if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "visible", Path: root, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	rec, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "visible", Harness: domain.HarnessClaudeCode, Kind: domain.KindWorker, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{RuntimeHandleID: "blocked", RuntimeLaunchID: "same-generation", WorkspacePath: root}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindSessionAgentAccount(ctx, rec.ID, domain.AgentAccountProviderClaude, "acct-a", now); err != nil {
		t.Fatal(err)
	}
	move := domain.SessionInterfaceTransition{ID: "unconfirmed", SessionID: rec.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, Policy: domain.SessionInterfaceTransitionDrain, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, SourceAccountID: "acct-a", TargetAccountID: "acct-b", Phase: domain.SessionInterfaceTransitionTargetStarting, CreatedAt: now, UpdatedAt: now}
	if _, _, err := db.CreateSessionInterfaceTransition(ctx, move); err != nil {
		t.Fatal(err)
	}
	for _, state := range []domain.AccountTransitionRuntimeState{domain.AccountTransitionRuntimeReserved, domain.AccountTransitionRuntimeStarted} {
		handle := ""
		if state == domain.AccountTransitionRuntimeStarted {
			handle = "blocked"
		}
		if ok, err := db.RecordAccountTransitionRuntime(ctx, move.ID, state, "same-generation", handle, now); err != nil || !ok {
			t.Fatalf("seed %s: %t %v", state, ok, err)
		}
	}
	if ok, err := db.AdvanceSessionInterfaceTransition(ctx, move.ID, move.Phase, domain.SessionInterfaceTransitionRecovery, "", "DAEMON_RESTARTED", "", now); err != nil || !ok {
		t.Fatalf("seed recovery: %t %v", ok, err)
	}
	m, _, rt, _, accounts, _ := newAccountTransitionManager(t, domain.SessionModeTUI)
	m.store = db
	m.SetAgentAccounts(sqliteRecoveryAccounts{transitionAccounts: accounts, db: db})
	rt.aliveByHandle = map[string]bool{"blocked": true}
	blocked := map[string]bool{"blocked": true}
	m.runtime = blockedRecoveryRuntime{transitionRuntime: rt, blocked: blocked}
	if _, err := m.recoverInterruptedInterfaceTransitions(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := db.GetSessionInterfaceTransition(ctx, move.ID)
	status, err := m.InterfaceTransitionStatus(ctx, rec.ID)
	if err != nil || saved.ErrorCode != "TARGET_STOP_UNCONFIRMED" || status.ReasonCode != "TARGET_STOP_UNCONFIRMED" || !m.SessionMutationInProgress(rec.ID) {
		t.Fatalf("unconfirmed recovery was invisible: saved=%+v status=%+v err=%v", saved, status, err)
	}
	if strings.Contains(saved.ErrorDetail+status.Reason, "private-stop-canary") || !strings.Contains(status.Reason, "restart AO") {
		t.Fatalf("unsafe or unactionable recovery classification: %s", status.Reason)
	}
	blocked["blocked"] = false
	if _, err := m.recoverInterruptedInterfaceTransitions(ctx); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.HasUnconfirmedAccountTransition(ctx, rec.ID); err != nil || pending || m.SessionMutationInProgress(rec.ID) {
		t.Fatalf("confirmed retry did not converge: pending=%t err=%v", pending, err)
	}
}
