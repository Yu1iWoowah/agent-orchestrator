package sessionmanager

import (
	"context"
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
