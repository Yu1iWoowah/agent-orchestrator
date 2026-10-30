package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestExplicitAccountRecoveryRetryKeepsFenceUntilStopConfirmed(t *testing.T) {
	ctx := context.Background()
	m, st, rt, _, _, _ := newAccountTransitionManager(t, domain.SessionModeTUI)
	rec := st.sessions["session-1"]
	rec.Metadata.RuntimeLaunchID = ""
	st.sessions[rec.ID] = rec
	now := time.Now()
	st.transitions["failed-target"] = domain.SessionInterfaceTransition{ID: "failed-target", SessionID: rec.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, SourceAccountID: "device-claude", TargetAccountID: "failed-account", Phase: domain.SessionInterfaceTransitionRecovery, ErrorCode: "TARGET_STOP_UNCONFIRMED", TargetRuntimeState: domain.AccountTransitionRuntimeStarted, TargetRuntimeLaunchID: "failed-launch", TargetRuntimeHandleID: "failed-target", CreatedAt: now}
	rt.aliveByHandle = map[string]bool{"failed-target": true, "other-session": true}
	blocked := map[string]bool{"failed-target": true}
	m.runtime = blockedRecoveryRuntime{transitionRuntime: rt, blocked: blocked}
	confirmed, err := m.RetryAccountRecovery(ctx, rec.ID)
	if err != nil || confirmed || !m.SessionMutationInProgress(rec.ID) {
		t.Fatalf("unconfirmed shutdown opened fence: confirmed=%v err=%v", confirmed, err)
	}
	if release, ok := m.AcquireSessionInput(rec.ID); ok {
		release()
		t.Fatal("input accepted while target stop unconfirmed")
	}
	blocked["failed-target"] = false
	confirmed, err = m.RetryAccountRecovery(ctx, rec.ID)
	if err != nil || !confirmed || m.SessionMutationInProgress(rec.ID) {
		t.Fatalf("confirmed shutdown stayed fenced: confirmed=%v err=%v", confirmed, err)
	}
	if rt.aliveByHandle["failed-target"] || !rt.aliveByHandle["other-session"] {
		t.Fatal("retry stopped the wrong writer")
	}
	saved, _, _ := st.GetSessionInterfaceTransition(ctx, "failed-target")
	if saved.TargetRuntimeState != domain.AccountTransitionRuntimeStopped {
		t.Fatal("shutdown has no durable receipt")
	}
	confirmed, err = m.RetryAccountRecovery(ctx, rec.ID)
	if err != nil || !confirmed {
		t.Fatalf("repeat retry is not idempotent: %v %v", confirmed, err)
	}
}

func TestExplicitRecoveryRetriesAnActiveUnconfirmedInterfaceTarget(t *testing.T) {
	ctx := context.Background()
	m, st, rt, _, _ := newTransitionManager(t, domain.SessionModeTUI)
	rec := st.sessions["session-1"]
	rec.Metadata.RuntimeHandleID = "failed-interface-target"
	st.sessions[rec.ID] = rec
	st.transitions["retained-interface"] = domain.SessionInterfaceTransition{
		ID: "retained-interface", SessionID: rec.ID, SourceMode: domain.SessionModeChat, TargetMode: domain.SessionModeTUI,
		Phase: domain.SessionInterfaceTransitionTargetStarting, ErrorCode: "TARGET_STOP_UNCONFIRMED", NativeConversationID: "native-1",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	rt.aliveByHandle = map[string]bool{"failed-interface-target": true, "other-session": true}
	rt.destroyErr = errors.New("target still alive")
	confirmed, err := m.RetryAccountRecovery(ctx, rec.ID)
	if err != nil || confirmed || !m.SessionMutationInProgress(rec.ID) {
		t.Fatalf("unconfirmed target must remain fenced: confirmed=%v err=%v", confirmed, err)
	}
	rt.destroyErr = nil
	confirmed, err = m.RetryAccountRecovery(ctx, rec.ID)
	if err != nil || !confirmed || m.SessionMutationInProgress(rec.ID) {
		t.Fatalf("confirmed target must release fence: confirmed=%v err=%v", confirmed, err)
	}
	if rt.aliveByHandle["failed-interface-target"] || !rt.aliveByHandle["other-session"] || rt.created != 0 {
		t.Fatal("retry changed another writer or launched a replacement")
	}
	if st.sessions[rec.ID].Mode != domain.SessionModeChat {
		t.Fatal("recovery did not restore source ownership")
	}
}
