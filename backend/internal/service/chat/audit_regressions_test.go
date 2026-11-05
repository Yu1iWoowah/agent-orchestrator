package chat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestAuditDeleteForkChecksActualSourceHome(t *testing.T) {
	svc, db, conv, _ := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	// The current parent lives in the protected home. A deferred cleanup record
	// was launched with a different account home before the parent changed.
	spy := &isolationConversation{recordCompactionConversation: conv}
	rows, _ := db.SideCompactions(t.Context(), true)
	svc.controllers["session"] = &Controller{conv: &nativeCleanupConversation{ChatConversation: spy, ChatForkDeleter: spy, home: rows[0].NativeHome}, harness: domain.HarnessCodex}
	runtime := svc.sides.runtimes["side"]
	var current frozenSideConfig
	if err := json.Unmarshal(runtime.side.LaunchConfig, &current); err != nil {
		t.Fatal(err)
	}
	svc.startConfigs[domain.SessionConversationOwner("session")] = StartConfig{Env: current.Env}
	cleanup := runtime.side
	cleanup.ID = "different-cleanup-owner"
	raw, err := json.Marshal(frozenSideConfig{Env: map[string]string{"CODEX_HOME": t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	cleanup.LaunchConfig = raw
	svc.sides.deleteRegisteredFork(context.Background(), cleanup, "provider-side-1")
	if spy.deletes != 0 {
		t.Fatalf("protected native DeleteFork reached actual parent provider: calls=%d", spy.deletes)
	}
}

func TestAuditSecondCompactionSameNativeKey(t *testing.T) {
	svc, db, _, home := newRecordHarness(t, domain.HarnessCodex)
	// Both handles already exist before the first protection record. The native
	// boundary permits attaching to a key while no compaction is unresolved.
	otherSide := addReviewSide(t, svc)
	otherSide.ProviderForkID = "provider-side-1"
	store := svc.sides.store.(*memorySideStore)
	store.mu.Lock()
	store.sides[otherSide.ID] = otherSide
	store.mu.Unlock()
	otherConv := &recordCompactionConversation{}
	svc.sides.runtimes[otherSide.ID] = &sideRuntime{side: otherSide, conv: otherConv, ctx: context.Background(), cancel: func() {}}
	if err := svc.nativeGuard.Run(t.Context(), domain.HarnessCodex, ports.ChatResumeConfig{ProviderConversationID: "provider-side-1", Env: map[string]string{"CODEX_HOME": home}}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	_, err := svc.CompactSideChat(t.Context(), otherSide.SessionID, otherSide.ID)
	rows, readErr := db.SideCompactions(t.Context(), true)
	if readErr != nil {
		t.Fatal(readErr)
	}
	t.Logf("second compaction count=%d unresolved=%d err=%v", otherConv.count, len(rows), err)
	second := svc.sides.runtimes[otherSide.ID]
	second.mu.Lock()
	fenced := second.compacting || second.compactionDispatching || second.outboundUnverified || second.compactionID != ""
	second.mu.Unlock()
	if fenced {
		t.Fatal("rejected second compaction retained its dispatch fence")
	}
	if rows[0].SideID != "side" {
		t.Fatal("replaced original obligation", rows)
	}

	if otherConv.count != 0 || len(rows) != 1 || !errors.Is(err, ErrNativeConversationProtected) {
		t.Fatalf("second compaction on protected key performed provider I/O: count=%d, unresolved=%d, err=%v", otherConv.count, len(rows), err)
	}
}

func TestAuditProtectedRetryAfterEventStreamDisconnect(t *testing.T) {
	svc, _, _, _ := newRecordHarness(t, domain.HarnessCodex)
	_, _ = svc.CompactSideChat(t.Context(), "session", "side")
	delete(svc.sides.runtimes, "side")
	conv := &protectedReplayConversation{recordCompactionConversation: &recordCompactionConversation{}, events: make(chan ports.ChatEvent), activated: make(chan string, 10)}
	driver := &protectedReconnectDriver{nativeCountingDriver: &nativeCountingDriver{}, conv: conv}
	reg := registry.New(driver)
	reg.SetNativeGuard(svc.nativeGuard)
	svc.drivers = reg
	if err := svc.RetrySideConnection(t.Context(), "session", "side"); err != nil {
		t.Fatal(err)
	}
	close(conv.events)
	deadline := time.Now().Add(time.Second)
	for driver.reconnected < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond * 10)
		_ = svc.RetrySideConnection(t.Context(), "session", "side")
	}
	svc.sides.cancel()
	if driver.reconnected != 2 {
		t.Fatalf("Retry never reattached after stream disconnect: Reconnect calls=%d", driver.reconnected)
	}
}

func TestAuditNoProtectedRowsResolveError(t *testing.T) {
	svc, _, _, _ := newRecordHarness(t, domain.HarnessCodex)
	invalid := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(invalid, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := svc.nativeGuard.Run(t.Context(), domain.HarnessCodex, ports.ChatResumeConfig{ProviderConversationID: "normal-thread", Env: map[string]string{"CODEX_HOME": invalid}}, func() error { called = true; return nil })
	if errors.Is(err, ErrNativeConversationProtected) {
		t.Fatalf("no protected rows but returned protected error: called=%v err=%v", called, err)
	}
}

func TestDeleteForkDefersWithoutActualHostHome(t *testing.T) {
	svc, _, conv, _ := newRecordHarness(t, domain.HarnessCodex)
	spy := &isolationConversation{recordCompactionConversation: conv}
	svc.controllers["session"] = &Controller{conv: &nativeCleanupConversation{ChatConversation: spy, ChatForkDeleter: spy, scopeErr: ports.ErrChatCompactionScopeNoHost}, harness: domain.HarnessCodex}
	store := svc.sides.store.(*memorySideStore)
	store.cleanup["fork"] = domain.SideProviderCleanup{ForkID: "fork", SessionID: "session", Harness: domain.HarnessCodex}
	svc.sides.deleteRegisteredFork(t.Context(), domain.SideConversation{ID: "cleanup", SessionID: "session", Harness: domain.HarnessCodex}, "fork")
	if spy.deletes != 0 || len(store.cleanup) != 1 {
		t.Fatal("unknown home did not retain deferred cleanup", spy.deletes, len(store.cleanup))
	}
}
