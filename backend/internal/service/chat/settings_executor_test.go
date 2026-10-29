package chat_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type journalCommands interface {
	QueueAgentSettings(context.Context, domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error)
	RunAgentSettings(context.Context) error
}
type settingsValidatorInstaller interface {
	SetAgentSettingsValidator(ports.ChatAgentSettingsValidator)
}

type journalValidator struct {
	denied  atomic.Bool
	version atomic.Int64
}

func (v *journalValidator) ValidateAgentSettings(_ context.Context, _ domain.AgentSettingsOperation, _ domain.AgentSettingsOwner, restoring bool) (domain.AgentSettingsQualification, error) {
	if v.denied.Load() && !restoring {
		return domain.AgentSettingsQualification{}, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	version := "inputs-1"
	if v.version.Load() != 0 {
		version = "inputs-2"
	}
	return domain.AgentSettingsQualification{InputFingerprint: version, PolicyVersion: "policy-1"}, nil
}

type journalConversation struct {
	*fakeConversation
	quiet         atomic.Bool
	settingsMu    sync.Mutex
	steps         []domain.AgentSettingsCheckpoint
	onStep        func(domain.AgentSettingsCheckpoint) error
	restoreFail   bool
	legacySetters atomic.Int64
	prepareCalls  atomic.Int64
	prepareError  atomic.Bool
	freshRecovery atomic.Bool
	restoredPlan  domain.AgentSettingsProviderPlan
}

func (c *journalConversation) CanHibernate(context.Context) (bool, error) { return c.quiet.Load(), nil }
func (*journalConversation) Hibernate() error                             { return nil }
func (c *journalConversation) PrepareAgentSettings(_ context.Context, source, target domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	c.prepareCalls.Add(1)
	if c.prepareError.Load() {
		return domain.AgentSettingsProviderPlan{}, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	return domain.AgentSettingsProviderPlan{Protocol: "fake", ProviderConversationID: c.ProviderConversationID(), Source: source, Target: target, Before: domain.AgentSettingsNativeValues{Model: "baseline", Effort: "low", Fast: "off"}, Permission: "default"}, nil
}
func (c *journalConversation) ApplyAgentSettingsStep(_ context.Context, plan domain.AgentSettingsProviderPlan, step domain.AgentSettingsCheckpoint, restoring bool) error {
	c.settingsMu.Lock()
	if restoring {
		c.restoredPlan = plan
	}
	c.steps = append(c.steps, step)
	callback := c.onStep
	fail := c.restoreFail
	c.settingsMu.Unlock()
	if callback != nil {
		if err := callback(step); err != nil {
			return err
		}
	}
	if restoring && fail {
		return errors.New("fake-restore-failed")
	}
	return nil
}

func (c *journalConversation) PrepareAgentSettingsRecovery(ctx context.Context, settings domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	if !c.freshRecovery.Load() {
		return domain.AgentSettingsProviderPlan{}, ports.ErrAgentSettingsCapabilitiesUnavailable
	}
	plan, err := c.PrepareAgentSettings(ctx, settings, settings)
	plan.Before.Model = "fresh-baseline"
	return plan, err
}
func (c *journalConversation) ListConfigOptions(context.Context) ([]ports.ChatConfigOption, error) {
	return nil, nil
}
func (c *journalConversation) SetConfigOption(context.Context, string, ports.ChatConfigOptionValue) ([]ports.ChatConfigOption, error) {
	c.legacySetters.Add(1)
	return nil, nil
}
func (c *journalConversation) stepCount() int {
	c.settingsMu.Lock()
	defer c.settingsMu.Unlock()
	return len(c.steps)
}

func journalHarness(t *testing.T) (*harness, *journalConversation, *journalValidator, journalCommands) {
	return journalHarnessWithStore(t, func(st *sqlite.Store) chatsvc.Store { return st })
}

func journalHarnessWithStore(t *testing.T, wrap func(*sqlite.Store) chatsvc.Store) (*harness, *journalConversation, *journalValidator, journalCommands) {
	t.Helper()
	conv := &journalConversation{fakeConversation: newFakeConversation()}
	conv.quiet.Store(true)
	h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
		if err := st.BindSessionAgentAccount(t.Context(), testSession, domain.AgentAccountProviderCodex, "device-codex", time.Now()); err != nil {
			t.Fatal(err)
		}
		return wrap(st)
	})
	record, _, err := h.st.GetSession(t.Context(), testSession)
	if err != nil {
		t.Fatal(err)
	}
	record.Metadata.ProviderConversationID = conv.ProviderConversationID()
	if err := h.st.UpdateSession(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	installer, ok := any(h.svc).(settingsValidatorInstaller)
	if !ok {
		t.Fatal("Chat has no settings validation boundary")
	}
	validator := &journalValidator{}
	installer.SetAgentSettingsValidator(validator)
	commands, ok := any(h.ctrl).(journalCommands)
	if !ok {
		t.Fatal("Chat has no durable settings scheduler")
	}
	return h, conv, validator, commands
}

type journalFaultStore struct {
	*sqlite.Store
	advanceAfter              domain.AgentSettingsCheckpoint
	advanceBefore             domain.AgentSettingsCheckpoint
	commitBefore, commitAfter bool
	once                      atomic.Bool
}

func (s *journalFaultStore) AdvanceAgentSettingsOperation(ctx context.Context, id domain.SessionID, opID string, version int64, checkpoint domain.AgentSettingsCheckpoint, at time.Time) (domain.AgentSettingsOperation, error) {
	if checkpoint == s.advanceBefore && s.once.CompareAndSwap(false, true) {
		return domain.AgentSettingsOperation{}, errors.New("before-checkpoint-fault")
	}
	op, err := s.Store.AdvanceAgentSettingsOperation(ctx, id, opID, version, checkpoint, at)
	if err == nil && checkpoint == s.advanceAfter && s.once.CompareAndSwap(false, true) {
		return domain.AgentSettingsOperation{}, errors.New("checkpoint-ack-lost")
	}
	return op, err
}
func (s *journalFaultStore) CommitAgentSettingsOperation(ctx context.Context, id domain.SessionID, opID string, at time.Time) (domain.AgentSettingsOperation, error) {
	if s.commitBefore && s.once.CompareAndSwap(false, true) {
		return domain.AgentSettingsOperation{}, errors.New("before-commit-fault")
	}
	op, err := s.Store.CommitAgentSettingsOperation(ctx, id, opID, at)
	if err == nil && s.commitAfter && s.once.CompareAndSwap(false, true) {
		return domain.AgentSettingsOperation{}, errors.New("commit-ack-lost")
	}
	return op, err
}

func TestSettingsSchedulerFaultsKeepDurableOutcomeAndDispatchConsistent(t *testing.T) {
	for _, fault := range []string{"before_model", "setter_model", "setter_effort", "setter_fast", "model_ack", "effort_ack", "fast_ack", "verifying_ack", "verified_ack", "commit_before", "commit_after", "rollback", "generation", "account", "native", "branch", "policy", "binary"} {
		t.Run(fault, func(t *testing.T) {
			h, conv, validator, commands := journalHarnessWithStore(t, func(st *sqlite.Store) chatsvc.Store {
				f := &journalFaultStore{Store: st, commitBefore: fault == "commit_before", commitAfter: fault == "commit_after"}
				f.advanceAfter = map[string]domain.AgentSettingsCheckpoint{"model_ack": domain.AgentSettingsApplyingModel, "effort_ack": domain.AgentSettingsApplyingEffort, "fast_ack": domain.AgentSettingsApplyingFast, "verifying_ack": domain.AgentSettingsVerifying, "verified_ack": domain.AgentSettingsVerified}[fault]
				if fault == "before_model" {
					f.advanceBefore = domain.AgentSettingsApplyingModel
				}
				return f
			})
			intent := journalIntent(t, h)
			conv.restoreFail = fault == "rollback"
			conv.onStep = func(step domain.AgentSettingsCheckpoint) error {
				saved, err := h.st.AgentSettingsOperation(t.Context(), testSession, intent.ID)
				if err != nil {
					return err
				}
				if saved.Checkpoint != step {
					t.Errorf("provider action preceded its checkpoint: %s versus %s", step, saved.Checkpoint)
				}
				if step == domain.AgentSettingsApplyingModel && fault == "generation" {
					record, _, err := h.st.GetSession(t.Context(), testSession)
					if err != nil {
						return err
					}
					record.Metadata.ControllerGeneration = "replacement"
					return h.st.UpdateSession(t.Context(), record)
				}
				if step == domain.AgentSettingsApplyingModel {
					switch fault {
					case "account":
						if err := h.st.CreateAgentAccount(t.Context(), domain.AgentAccount{ID: "synthetic-account", Provider: domain.AgentAccountProviderCodex, Home: t.TempDir(), ProfileKind: domain.AgentAccountProfileExternal, CreatedAt: time.Now()}); err != nil {
							return err
						}
						return h.st.BindSessionAgentAccount(t.Context(), testSession, domain.AgentAccountProviderCodex, "synthetic-account", time.Now())
					case "native":
						record, _, err := h.st.GetSession(t.Context(), testSession)
						if err != nil {
							return err
						}
						record.Metadata.ProviderConversationID = "replacement-native"
						return h.st.UpdateSession(t.Context(), record)
					case "branch":
						return h.st.CreateAndActivateConversationBranch(t.Context(), testSession, domain.ConversationBranch{ID: "replacement-branch", ConversationID: h.ctrl.ConversationID(), SessionID: testSession, ParentBranchID: intent.BranchID, ProviderConversationID: conv.ProviderConversationID(), CreatedAt: time.Now()}, "replacement-generation", time.Now())
					case "policy":
						validator.denied.Store(true)
					case "binary":
						validator.version.Store(1)
					}
				}
				failedStep := map[string]domain.AgentSettingsCheckpoint{"setter_model": domain.AgentSettingsApplyingModel, "setter_effort": domain.AgentSettingsApplyingEffort, "setter_fast": domain.AgentSettingsApplyingFast, "rollback": domain.AgentSettingsApplyingFast}[fault]
				if step == failedStep {
					return errors.New("after-setter-fault")
				}
				return nil
			}
			op, err := commands.QueueAgentSettings(t.Context(), intent)
			if fault == "generation" || fault == "rollback" || fault == "account" || fault == "native" || fault == "branch" || fault == "binary" {
				if err == nil || op.Phase != domain.AgentSettingsRecoveryRequired {
					t.Fatalf("uncertain owner/provider did not retain explicit recovery fence: %+v %v", op, err)
				}
				if fault == "rollback" {
					turn, sendErr := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "keep while recovery required"})
					if sendErr != nil || turn.State != domain.TurnStateQueued || len(conv.sentTexts()) != 0 {
						t.Fatalf("recovery fence leaked turn: %+v %v", turn, sendErr)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantPhase := domain.AgentSettingsFailed
			wantRevision := int64(0)
			wantSettings := intent.Source
			if fault == "commit_after" {
				wantPhase = domain.AgentSettingsApplied
				wantRevision = 1
				wantSettings = intent.Target
			}
			if op.Phase != wantPhase {
				t.Fatalf("durable outcome lost: %+v", op)
			}
			turn, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "after safe outcome"})
			if err != nil {
				t.Fatal(err)
			}
			saved, err := h.st.TurnByID(t.Context(), turn.ID)
			if err != nil || saved.AgentSettings == nil || saved.AgentSettings.Revision == nil || *saved.AgentSettings.Revision != wantRevision || *saved.AgentSettings.Settings != wantSettings {
				t.Fatalf("fault changed dispatch revision: %+v %v", saved, err)
			}
			if fault == "commit_after" {
				conv.settingsMu.Lock()
				defer conv.settingsMu.Unlock()
				for _, step := range conv.steps {
					if step == domain.AgentSettingsRollingBack {
						t.Fatal("lost commit acknowledgement rolled back an applied selection")
					}
				}
			}
		})
	}
}

func journalIntent(t *testing.T, h *harness) domain.AgentSettingsOperation {
	t.Helper()
	current, err := h.st.SessionAgentSettings(t.Context(), testSession)
	if err != nil {
		t.Fatal(err)
	}
	return domain.AgentSettingsOperation{ID: "settings-operation", SessionID: testSession, IdempotencyKey: "settings-key", Source: current.Settings, Target: domain.AgentSettings{Model: "new-model", Effort: "high", FastMode: domain.AgentFastOff}, BaseRevision: current.Revision, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID, Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}, CreatedAt: h.now(), ApplyMode: domain.AgentSettingsAfterTurn}
}

func TestSettingsSchedulerBusyDefersSettersAndPrecedesQueuedRelay(t *testing.T) {
	h, conv, _, commands := journalHarness(t)
	first, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "current turn"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := commands.QueueAgentSettings(t.Context(), journalIntent(t, h))
	if err != nil || op.Phase != domain.AgentSettingsPending || conv.stepCount() != 0 {
		t.Fatalf("busy turn changed settings: %+v %v steps=%d", op, err, conv.stepCount())
	}
	if _, err := commands.QueueAgentSettings(t.Context(), journalIntent(t, h)); err != nil {
		t.Fatalf("idempotent busy request: %v", err)
	}
	other := journalIntent(t, h)
	other.ID, other.IdempotencyKey = "other", "other"
	if _, err := commands.QueueAgentSettings(t.Context(), other); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("second pending writer accepted: %v", err)
	}
	if err := h.ctrl.SetSettings(t.Context(), domain.ConversationSettings{Model: "bypass"}); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("legacy settings bypassed journal: %v", err)
	}
	if _, err := h.svc.SetConfigOption(t.Context(), testSession, "model", ports.ChatConfigOptionValue{Select: "bypass"}); !errors.Is(err, ports.ErrAgentSettingsConflict) || conv.legacySetters.Load() != 0 {
		t.Fatalf("legacy native setter bypassed journal: %v", err)
	}
	second, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "queued unchanged"})
	if err != nil || second.State != domain.TurnStateQueued {
		t.Fatalf("queue: %+v %v", second, err)
	}
	if _, err := h.svc.RelayChatTurn(t.Context(), testSession, "relay unchanged"); err != nil {
		t.Fatal(err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: first.ProviderTurnID, TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(store.ConversationSnapshot) bool {
		saved, err := h.st.AgentSettingsOperation(t.Context(), testSession, op.ID)
		return err == nil && saved.Phase == domain.AgentSettingsApplied && len(conv.sentTexts()) == 2
	})
	saved, err := h.st.TurnByID(t.Context(), second.ID)
	if err != nil || saved.AgentSettings == nil || saved.AgentSettings.Revision == nil || *saved.AgentSettings.Revision != 1 || saved.AgentSettings.Settings == nil || *saved.AgentSettings.Settings != op.Target {
		t.Fatalf("queued wire/revision diverged: %+v %v", saved, err)
	}
	conv.mu.Lock()
	defer conv.mu.Unlock()
	if conv.sent[0].Settings.Model != "" || conv.sent[1].Settings.Model != "new-model" || conv.sent[1].Settings.FastMode != domain.AgentFastOff {
		t.Fatalf("busy or next wire changed incorrectly: %+v", conv.sent)
	}
	batch := parseDeliveredBatch(t, conv.sent[1].Text)
	if len(batch) != 2 || batch[0].Text != "queued unchanged" || batch[1].Text != "relay unchanged" {
		t.Fatalf("messages changed or lost: %+v", batch)
	}
}

func TestSettingsSchedulerRequiresPositiveQuiescenceAndRevalidates(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "quiet", true: "policy_or_inputs_changed"}[denied], func(t *testing.T) {
			h, conv, validator, commands := journalHarness(t)
			conv.quiet.Store(false)
			op, err := commands.QueueAgentSettings(t.Context(), journalIntent(t, h))
			if err != nil || op.Phase != domain.AgentSettingsPending || conv.stepCount() != 0 {
				t.Fatalf("unproved idle ran setters: %+v %v", op, err)
			}
			turn, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "retain while fenced"})
			if err != nil || turn.State != domain.TurnStateQueued || len(conv.sentTexts()) != 0 {
				t.Fatalf("fence lost prompt or sent early: %+v %v", turn, err)
			}
			validator.denied.Store(denied)
			conv.quiet.Store(true)
			if err := commands.RunAgentSettings(t.Context()); err != nil {
				t.Fatal(err)
			}
			op, err = h.st.AgentSettingsOperation(t.Context(), testSession, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if denied {
				if op.Phase != domain.AgentSettingsFailed || conv.stepCount() != 0 {
					t.Fatalf("changed proof was used: %+v", op)
				}
			} else if op.Phase != domain.AgentSettingsApplied {
				t.Fatalf("quiet operation not applied: %+v", op)
			}
			if len(conv.sentTexts()) != 1 {
				t.Fatal("fenced prompt was not drained after safe outcome")
			}
		})
	}
}

func TestSettingsRecoveryUsesFreshBaselineWhenDiscoveryInputsChange(t *testing.T) {
	h, conv, validator, commands := journalHarness(t)
	conv.freshRecovery.Store(true)
	conv.onStep = func(step domain.AgentSettingsCheckpoint) error {
		if step == domain.AgentSettingsApplyingModel {
			validator.version.Store(1)
		}
		return nil
	}
	intent := journalIntent(t, h)
	op, err := commands.QueueAgentSettings(t.Context(), intent)
	if err != nil || op.Phase != domain.AgentSettingsFailed {
		t.Fatalf("new qualified baseline could not finish recovery: %+v %v", op, err)
	}
	conv.settingsMu.Lock()
	restored := conv.restoredPlan
	conv.settingsMu.Unlock()
	if restored.Before.Model != "fresh-baseline" || restored.Source != intent.Source || restored.Target != intent.Source {
		t.Fatalf("recovery borrowed old discovery preimage: %+v", restored)
	}
	if _, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "after fresh baseline restoration"}); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsSchedulerCancelReleasesUnstartedSlot(t *testing.T) {
	h, conv, _, commands := journalHarness(t)
	conv.quiet.Store(false)
	op, err := commands.QueueAgentSettings(t.Context(), journalIntent(t, h))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := h.ctrl.CancelAgentSettings(t.Context(), op.ID, op.Version)
	if err != nil || cancelled.Phase != domain.AgentSettingsCancelled || conv.stepCount() != 0 {
		t.Fatalf("unsafe cancel: %+v %v", cancelled, err)
	}
	next := journalIntent(t, h)
	next.ID, next.IdempotencyKey = "next-operation", "next-key"
	if _, err := commands.QueueAgentSettings(t.Context(), next); err != nil {
		t.Fatalf("cancel kept pending slot: %v", err)
	}
}
