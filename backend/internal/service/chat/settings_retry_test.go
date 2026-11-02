package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type settingsClockTimer struct {
	at time.Time
	ch chan time.Time
}

type settingsTestClock struct {
	mu        sync.Mutex
	now       time.Time
	timers    []settingsClockTimer
	scheduled chan time.Duration
}

func newSettingsTestClock() *settingsTestClock {
	return &settingsTestClock{now: time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC), scheduled: make(chan time.Duration, 1024)}
}

func (c *settingsTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *settingsTestClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, settingsClockTimer{at: c.now.Add(d), ch: ch})
	c.mu.Unlock()
	c.scheduled <- d
	return ch
}

func (c *settingsTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	remaining := c.timers[:0]
	for _, timer := range c.timers {
		if !timer.at.After(c.now) {
			timer.ch <- c.now
		} else {
			remaining = append(remaining, timer)
		}
	}
	c.timers = remaining
}

func (c *settingsTestClock) awaitTimer(t *testing.T, d time.Duration) {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case got := <-c.scheduled:
			if got == d {
				return
			}
		case <-timeout.C:
			t.Fatalf("timer %s was not scheduled", d)
		}
	}
}

func retrySettingsHarness(t *testing.T, conv ports.ChatConversation, clock *settingsTestClock) *harness {
	t.Helper()
	h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *sqlite.Store) chatsvc.Store {
		if err := st.BindSessionAgentAccount(t.Context(), testSession, domain.AgentAccountProviderCodex, "device-codex", clock.Now()); err != nil {
			t.Fatal(err)
		}
		return st
	}, domain.HarnessCodex, func(options *chatsvc.Options) { options.Now = clock.Now; options.After = clock.After })
	rec, _, err := h.st.GetSession(t.Context(), testSession)
	if err != nil {
		t.Fatal(err)
	}
	rec.Metadata.ProviderConversationID = conv.ProviderConversationID()
	if err = h.st.UpdateSession(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	h.svc.SetAgentSettingsValidator(&journalValidator{})
	return h
}

func TestSettingsRetriesAreBoundedAndManualRetryPreservesIntent(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &journalConversation{fakeConversation: newFakeConversation()}
	conv.quiet.Store(true)
	conv.prepareError.Store(true)
	h := retrySettingsHarness(t, conv, clock)
	op, err := h.ctrl.QueueAgentSettings(t.Context(), journalIntent(t, h))
	if !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) || op.Phase != domain.AgentSettingsPending {
		t.Fatalf("durable transient failure: %+v %v", op, err)
	}
	for _, delay := range []time.Duration{time.Second, 5 * time.Second, 15 * time.Second} {
		clock.awaitTimer(t, delay)
		clock.advance(delay)
	}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		attempts, err := h.st.ListAgentSettingsReconciliations(t.Context(), testSession)
		return err == nil && len(attempts) == 1 && attempts[0].RetryCount == 3 && attempts[0].Phase == "blocked" && conv.prepareCalls.Load() == 4
	})
	if got := conv.prepareCalls.Load(); got != 4 {
		t.Fatalf("prepare calls=%d want initial+3", got)
	}
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "held behind retry limit", ClientMessageID: "retry-limit-input"})
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("intake after limit: %+v %v", turn, err)
	}
	conv.prepareError.Store(false)
	oldAttempts, _ := h.st.ListAgentSettingsReconciliations(t.Context(), testSession)
	original := oldAttempts[0]
	retried, err := h.svc.RetryAgentSettings(t.Context(), testSession, op.ID, original.ID, original.Version, original.Revision)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := h.svc.RetryAgentSettings(t.Context(), testSession, op.ID, original.ID, original.Version, original.Revision)
	if err != nil || replay.ID != retried.ID {
		t.Fatalf("retry replay: %+v %v", replay, err)
	}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool { return len(conv.sentMessages()) == 1 })
	saved, err := h.st.AgentSettingsOperation(t.Context(), testSession, op.ID)
	if err != nil || saved.Phase != domain.AgentSettingsApplied || saved.Target != op.Target {
		t.Fatalf("manual retry changed intent: %+v %v", saved, err)
	}
	attempts, err := h.st.ListAgentSettingsReconciliations(t.Context(), testSession)
	if err != nil || len(attempts) != 2 || attempts[0].ID == attempts[1].ID {
		t.Fatalf("retry history: %+v %v", attempts, err)
	}
}

type settingsDeadlinePrepare struct {
	*journalConversation
	entered chan struct{}
	once    sync.Once
}

func (c *settingsDeadlinePrepare) PrepareAgentSettings(ctx context.Context, _, _ domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	c.once.Do(func() { close(c.entered) })
	<-ctx.Done()
	return domain.AgentSettingsProviderPlan{}, ctx.Err()
}

func TestSettingsReadDeadlineUsesFakeClockAndKeepsPendingInput(t *testing.T) {
	clock := newSettingsTestClock()
	base := &journalConversation{fakeConversation: newFakeConversation()}
	base.quiet.Store(true)
	conv := &settingsDeadlinePrepare{journalConversation: base, entered: make(chan struct{})}
	h := retrySettingsHarness(t, conv, clock)
	result := make(chan error, 1)
	go func() { _, err := h.ctrl.QueueAgentSettings(t.Context(), journalIntent(t, h)); result <- err }()
	select {
	case <-conv.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("prepare not entered")
	}
	clock.advance(10 * time.Second)
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read exceeded deadline")
	}
	attempts, err := h.st.ListAgentSettingsReconciliations(t.Context(), testSession)
	if err != nil || len(attempts) != 1 || attempts[0].EffectPending || attempts[0].SafeCause != "timeout" || attempts[0].FailedStep != "preparing" {
		t.Fatalf("read timeout diagnosis: %+v %v", attempts, err)
	}
	if len(base.sentMessages()) != 0 || base.stepCount() != 0 {
		t.Fatal("read timeout crossed provider mutation boundary")
	}
}

type stickySettingsEvidence struct {
	*journalConversation
	current domain.AgentSettings
}

func (c *stickySettingsEvidence) ReconciliationEvidence(_ context.Context, owner domain.AgentSettingsOwner) (ports.AgentSettingsReconciliationEvidence, error) {
	current := c.current
	return ports.AgentSettingsReconciliationEvidence{Kind: "provider_current", Owner: owner, ProviderEpoch: "controlled-native-epoch", SourceRef: "controlled-current-receipt", Settings: &current, PermissionVerified: true}, nil
}

func TestSettingsRestartKeepsUnknownActionAndSafeNumericFailure(t *testing.T) {
	base := &journalConversation{fakeConversation: newFakeConversation()}
	base.quiet.Store(true)
	conv := &stickySettingsEvidence{journalConversation: base, current: domain.AgentSettings{FastMode: domain.AgentFastDefault}}
	h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store {
		if err := st.BindSessionAgentAccount(t.Context(), testSession, domain.AgentAccountProviderCodex, "device-codex", time.Now()); err != nil {
			t.Fatal(err)
		}
		return st
	}, domain.HarnessCodex)
	rec, _, _ := h.st.GetSession(t.Context(), testSession)
	rec.Metadata.ProviderConversationID = conv.ProviderConversationID()
	if err := h.st.UpdateSession(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	validator := &journalValidator{}
	h.svc.SetAgentSettingsValidator(validator)
	intent := journalIntent(t, h)
	conv.current = intent.Source
	base.onStep = func(step domain.AgentSettingsCheckpoint) error {
		if step == domain.AgentSettingsApplyingModel {
			return ports.NewAgentSettingsRPCFailure(-32603, errors.New("private-provider-error-token-path-sentinel"))
		}
		return nil
	}
	_, _ = h.ctrl.QueueAgentSettings(t.Context(), intent)
	attempts, err := h.st.ListAgentSettingsReconciliations(t.Context(), testSession)
	if err != nil || len(attempts) != 1 || !attempts[0].EffectPending || attempts[0].ActionKey == "" || attempts[0].JSONRPCCode == nil || *attempts[0].JSONRPCCode != -32603 {
		t.Fatalf("unknown setter checkpoint lost: %+v %v", attempts, err)
	}
	original := attempts[0]
	if original.Desired != intent.Target || original.PreferenceAtStart != intent.Source {
		t.Fatalf("desired/source mixed: %+v", original)
	}
	raw, err := json.Marshal(original)
	if err != nil || strings.Contains(string(raw), "private-provider-error") {
		t.Fatalf("unsafe diagnosis: %s %v", raw, err)
	}
	replacementBase := &journalConversation{fakeConversation: newFakeConversation()}
	replacementBase.quiet.Store(true)
	replacement := &stickySettingsEvidence{journalConversation: replacementBase, current: intent.Source}
	controller := restartJournalController(t, h, validator, replacement)
	if err := controller.RunAgentSettings(t.Context()); err == nil {
		t.Fatal("manual retry crossed unknown effect")
	}
	if replacementBase.stepCount() != 0 {
		t.Fatal("restart repeated a possibly applied native setter")
	}
	after, _ := h.st.ListAgentSettingsReconciliations(t.Context(), testSession)
	if len(after) != 1 || after[0].ID != original.ID || after[0].ActionKey != original.ActionKey || !after[0].EffectPending {
		t.Fatalf("restart replaced original uncertainty: %+v", after)
	}
}

func TestCancelledIntentCannotLendItsAttemptToNextPreference(t *testing.T) {
	h, conv, _, commands := journalHarness(t)
	conv.quiet.Store(false)
	first, err := commands.QueueAgentSettings(t.Context(), journalIntent(t, h))
	if err != nil || first.Phase != domain.AgentSettingsPending {
		t.Fatalf("pending: %+v %v", first, err)
	}
	if _, err = h.ctrl.CancelAgentSettings(t.Context(), first.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	second := journalIntent(t, h)
	second.ID = "after-cancel"
	second.IdempotencyKey = "after-cancel-key"
	second.Target.Model = "second-model"
	conv.quiet.Store(true)
	applied, err := commands.QueueAgentSettings(t.Context(), second)
	if err != nil || applied.Phase != domain.AgentSettingsApplied {
		t.Fatalf("new intent reused cancelled work: %+v %v", applied, err)
	}
	attempts, err := h.st.ListAgentSettingsReconciliations(t.Context(), testSession)
	if err != nil || len(attempts) != 2 || attempts[0].OperationID != second.ID || attempts[0].Desired != second.Target || attempts[1].OperationID != first.ID || attempts[1].Phase != "cancelled" {
		t.Fatalf("attempt identity/history: %+v %v", attempts, err)
	}
}
