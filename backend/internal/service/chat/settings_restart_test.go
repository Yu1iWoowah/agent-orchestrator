package chat_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func restartJournalController(t *testing.T, h *harness, validator *journalValidator, provider *journalConversation) *chatsvc.Controller {
	t.Helper()
	if err := h.svc.Stop(t.Context(), testSession); err != nil {
		t.Fatal(err)
	}
	var sequence atomic.Int64
	svc := chatsvc.New(chatsvc.Options{Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st), PageReader: boundedSnapshotReader(h.st), Drivers: fakeRegistry{driver: fakeDriver{conv: provider}}, Activity: h.activity, Log: slog.New(slog.DiscardHandler), Now: h.now, NewID: func() string { return fmt.Sprintf("restart-%d", sequence.Add(1)) }})
	svc.SetAgentSettingsValidator(validator)
	controller, err := svc.Start(t.Context(), chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(), ProviderConversationID: provider.ProviderConversationID()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })
	return controller
}

func TestSettingsJournalRestartReconcilesSameOperationBeforeDispatch(t *testing.T) {
	for _, stage := range []domain.AgentSettingsCheckpoint{domain.AgentSettingsQueued, domain.AgentSettingsApplyingModel, domain.AgentSettingsApplyingEffort, domain.AgentSettingsApplyingFast, domain.AgentSettingsVerifying, domain.AgentSettingsVerified} {
		t.Run(string(stage), func(t *testing.T) {
			h, provider, validator, _ := journalHarness(t)
			intent := journalIntent(t, h)
			op, err := h.st.BeginAgentSettingsOperation(t.Context(), intent)
			if err != nil {
				t.Fatal(err)
			}
			current, err := h.st.SessionAgentSettings(t.Context(), testSession)
			if err != nil {
				t.Fatal(err)
			}
			owner := domain.AgentSettingsOwner{SessionID: testSession, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID}
			plan, err := provider.PrepareAgentSettings(t.Context(), op.Source, op.Target)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := validator.ValidateAgentSettings(t.Context(), op, owner, false)
			if err != nil {
				t.Fatal(err)
			}
			op, err = h.st.PinAgentSettingsExecution(t.Context(), testSession, op.ID, op.Version, domain.AgentSettingsExecution{Owner: owner, Revision: current.Revision, Qualification: proof, Plan: plan}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if stage != domain.AgentSettingsQueued {
				checkpoint := stage
				if checkpoint == domain.AgentSettingsVerified {
					checkpoint = domain.AgentSettingsVerifying
				}
				op, err = h.st.AdvanceAgentSettingsOperation(t.Context(), testSession, op.ID, op.Version, checkpoint, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if stage == domain.AgentSettingsVerified {
					op, err = h.st.AdvanceAgentSettingsOperation(t.Context(), testSession, op.ID, op.Version, stage, time.Now())
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			replacement := &journalConversation{fakeConversation: newFakeConversation()}
			replacement.quiet.Store(true)
			controller := restartJournalController(t, h, validator, replacement)
			saved, err := h.st.AgentSettingsOperation(t.Context(), testSession, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.AgentSettingsFailed
			if stage == domain.AgentSettingsQueued {
				want = domain.AgentSettingsApplied
			}
			if saved.Phase != want || saved.ControllerGeneration != intent.ControllerGeneration {
				t.Fatalf("restart did not reconcile original journal before publication: %+v", saved)
			}
			if controller.Generation() == intent.ControllerGeneration {
				t.Fatal("replacement reused old generation")
			}
			if stage != domain.AgentSettingsQueued {
				if saved.Recovery == nil || saved.Recovery.Owner.ControllerGeneration != controller.Generation() || saved.Recovery.Owner.BranchID != current.BranchID {
					t.Fatalf("restoration did not use current qualified owner: %+v", saved.Recovery)
				}
			}
			turn, err := controller.Send(t.Context(), ports.ChatUserMessage{Text: "after restart"})
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := h.st.TurnByID(t.Context(), turn.ID)
			if err != nil || receipt.AgentSettings == nil || receipt.AgentSettings.Revision == nil {
				t.Fatalf("unqualified replacement dispatch: %+v %v", receipt, err)
			}
			wantSettings := intent.Source
			wantRevision := int64(0)
			if stage == domain.AgentSettingsQueued {
				wantSettings = intent.Target
				wantRevision = 1
			}
			if *receipt.AgentSettings.Settings != wantSettings || *receipt.AgentSettings.Revision != wantRevision {
				t.Fatalf("restart used failed target: %+v", receipt.AgentSettings)
			}
		})
	}
}

func TestSettingsCommittedColdResumeRepreparesWireOrKeepsFence(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("baseline_unavailable_%t", unavailable), func(t *testing.T) {
			h, _, validator, commands := journalHarness(t)
			op, err := commands.QueueAgentSettings(t.Context(), journalIntent(t, h))
			if err != nil || op.Phase != domain.AgentSettingsApplied {
				t.Fatalf("apply: %+v %v", op, err)
			}
			replacement := &journalConversation{fakeConversation: newFakeConversation()}
			replacement.quiet.Store(true)
			replacement.prepareError.Store(unavailable)
			replacement.freshRecovery.Store(true)
			controller := restartJournalController(t, h, validator, replacement)
			if replacement.prepareCalls.Load() != 1 {
				t.Fatal("cold resume reused sticky thread settings without proving a baseline")
			}
			turn, err := controller.Send(t.Context(), ports.ChatUserMessage{Text: "keep paid choice explicit"})
			if err != nil {
				t.Fatal(err)
			}
			if unavailable {
				if turn.State != domain.TurnStateQueued || len(replacement.sentTexts()) != 0 {
					t.Fatalf("unknown baseline allowed dispatch: %+v", turn)
				}
				replacement.prepareError.Store(false)
				if _, err := controller.Send(t.Context(), ports.ChatUserMessage{Text: "resume after proof"}); err != nil {
					t.Fatal(err)
				}
			}
			if len(replacement.sentTexts()) != 1 {
				t.Fatal("cold resume lost or duplicated prompt")
			}
			replacement.mu.Lock()
			defer replacement.mu.Unlock()
			if replacement.sent[0].Settings.FastMode != domain.AgentFastOff || replacement.sent[0].Settings.Model != "new-model" {
				t.Fatalf("cold resume reverted explicit choice: %+v", replacement.sent[0].Settings)
			}
		})
	}
}

func TestSettingsColdReplayRechecksInputsAfterFinalProviderStep(t *testing.T) {
	h, _, validator, commands := journalHarness(t)
	if op, err := commands.QueueAgentSettings(t.Context(), journalIntent(t, h)); err != nil || op.Phase != domain.AgentSettingsApplied {
		t.Fatalf("apply: %+v %v", op, err)
	}
	replacement := &journalConversation{fakeConversation: newFakeConversation()}
	replacement.quiet.Store(true)
	var changed atomic.Bool
	replacement.freshRecovery.Store(true)
	replacement.onStep = func(step domain.AgentSettingsCheckpoint) error {
		if step == domain.AgentSettingsVerifying && changed.CompareAndSwap(false, true) {
			validator.version.Store(1)
		}
		return nil
	}
	controller := restartJournalController(t, h, validator, replacement)
	if _, err := controller.Send(t.Context(), ports.ChatUserMessage{Text: "after startup input change"}); err != nil {
		t.Fatal(err)
	}
	if replacement.prepareCalls.Load() != 2 {
		t.Fatal("cold replay dispatched using inputs invalidated during final provider step")
	}
}

func TestSettingsCommittedColdResumeDoesNotRequireRetiredSource(t *testing.T) {
	current := domain.AgentSettings{Model: "current", Effort: "low", FastMode: domain.AgentFastOff}
	for _, tc := range []struct {
		name         string
		old, current domain.AgentSettings
	}{
		{"model", domain.AgentSettings{Model: "retired", Effort: "high", FastMode: domain.AgentFastOn}, current},
		{"effort", domain.AgentSettings{Model: "current", Effort: "high", FastMode: domain.AgentFastOff}, current},
		{"fast", domain.AgentSettings{Model: "current", Effort: "low", FastMode: domain.AgentFastOn}, current},
		{"defaults", domain.AgentSettings{Model: "retired", Effort: "high", FastMode: domain.AgentFastOn}, domain.AgentSettings{FastMode: domain.AgentFastDefault}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, validator, commands := journalHarness(t)
			first := journalIntent(t, h)
			first.Target = tc.old
			if op, err := commands.QueueAgentSettings(t.Context(), first); err != nil || op.Phase != domain.AgentSettingsApplied {
				t.Fatalf("first selection: %+v %v", op, err)
			}
			intent := journalIntent(t, h)
			intent.ID, intent.IdempotencyKey, intent.Target = "committed-current", "current-key", tc.current
			if op, err := commands.QueueAgentSettings(t.Context(), intent); err != nil || op.Phase != domain.AgentSettingsApplied {
				t.Fatalf("current selection: %+v %v", op, err)
			}
			replacement := &journalConversation{fakeConversation: newFakeConversation(), validSource: &tc.current}
			replacement.quiet.Store(true)
			replacement.freshRecovery.Store(true)
			validator.currentOnly.Store(true)
			controller := restartJournalController(t, h, validator, replacement)
			turn, err := controller.Send(t.Context(), ports.ChatUserMessage{Text: "after source retirement"})
			if err != nil || turn.State != domain.TurnStateRunning || len(replacement.sentTexts()) != 1 {
				t.Fatalf("valid committed preference remained fenced by retired source: %+v %v", turn, err)
			}
			receipt, err := h.st.TurnByID(t.Context(), turn.ID)
			if err != nil || receipt.AgentSettings == nil || receipt.AgentSettings.Revision == nil || *receipt.AgentSettings.Revision != 2 || receipt.AgentSettings.Settings == nil || *receipt.AgentSettings.Settings != tc.current {
				t.Fatalf("cold replay lost current settings/revision: %+v %v", receipt, err)
			}
			saved, err := h.st.AgentSettingsOperation(t.Context(), testSession, intent.ID)
			if err != nil || saved.Phase != domain.AgentSettingsApplied || saved.Source != tc.old || saved.Target != tc.current {
				t.Fatalf("cold replay rewrote completed history: %+v %v", saved, err)
			}
		})
	}
}
