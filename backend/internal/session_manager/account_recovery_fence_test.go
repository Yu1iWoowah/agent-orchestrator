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
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Read the production binding history rather than mirroring its current value
// into the account fake; recovery and the store must see the same commit.
type sqliteRecoveryAccounts struct {
	*transitionAccounts
	db *sqlitestore.Store
}

func (a sqliteRecoveryAccounts) SessionAccountID(ctx context.Context, id domain.SessionID, harness domain.AgentHarness) (string, error) {
	provider, ok := domain.AgentAccountProviderFor(harness)
	if !ok {
		return "", ports.ErrAgentAccountUnknown
	}
	account, found, err := a.db.GetSessionAgentAccount(ctx, id, provider)
	if err != nil {
		return "", err
	}
	if !found {
		return "", ports.ErrAgentAccountUnknown
	}
	return account, nil
}

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
	for _, tc := range []struct {
		name           string
		reverse, exact bool
	}{
		{name: "blocked first"}, {name: "settled first", reverse: true}, {name: "exact deferred", exact: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, st, rt, _, _, _ := newAccountTransitionManager(t, domain.SessionModeTUI)
			m.store = orderedRecoveryStore{transitionStore: st, reverse: tc.reverse}
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
			var err error
			if tc.exact {
				_, err = m.recoverInterfaceTransitions(ctx, "a-blocked")
			} else {
				_, err = m.recoverInterruptedInterfaceTransitions(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			pending, _, _ := st.GetSessionInterfaceTransition(ctx, "a-blocked")
			settled, _, _ := st.GetSessionInterfaceTransition(ctx, "b-settled")
			wantSettled := domain.AccountTransitionRuntimeStopped
			if tc.exact {
				wantSettled = domain.AccountTransitionRuntimeStarted
			}
			if !m.SessionMutationInProgress(rec.ID) || pending.TargetRuntimeState != domain.AccountTransitionRuntimeStarted || settled.TargetRuntimeState != wantSettled || pending.ErrorCode != "TARGET_STOP_UNCONFIRMED" {
				t.Fatalf("receipt fences overwrote each other: pending=%+v settled=%+v", pending, settled)
			}
			if tc.exact && settled.ErrorCode != "" {
				t.Errorf("unattempted sibling was called a shutdown failure: %+v", settled)
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
	before, _, err := db.GetSessionInterfaceTransition(ctx, move.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.recoverInterruptedInterfaceTransitions(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := db.GetSessionInterfaceTransition(ctx, move.ID)
	if !saved.CompletedAt.Equal(before.CompletedAt) || !saved.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("classification rewrote historical timestamps: before=%+v after=%+v", before, saved)
	}
	status, err := m.InterfaceTransitionStatus(ctx, rec.ID)
	if err != nil || saved.ErrorCode != "TARGET_STOP_UNCONFIRMED" || status.ReasonCode != "TARGET_STOP_UNCONFIRMED" || !m.SessionMutationInProgress(rec.ID) {
		t.Fatalf("unconfirmed recovery was invisible: saved=%+v status=%+v err=%v", saved, status, err)
	}
	if strings.Contains(saved.ErrorDetail+status.Reason, "private-stop-canary") || !strings.Contains(status.Reason, "automatically retry") || !strings.Contains(status.Reason, "next time it starts") {
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
