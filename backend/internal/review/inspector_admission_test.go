package review

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestInspectorAdmissionUsesNewerReviewerPin(t *testing.T) {
	for _, harness := range []domain.ReviewerHarness{domain.ReviewerCodex, domain.ReviewerClaudeCode} {
		for _, entry := range []string{"switch", "next-pass PATCH"} {
			for _, stale := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/LKG=%t", harness, entry, stale), func(t *testing.T) {
					ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
					worker.ReviewerHarness = harness
					worker.ReviewerConfig = domain.AgentConfig{Model: "review-a"}
					seedReviewWorker(t, store, worker)
					launcher := &fakeLauncher{handle: "review-pane", agentSessionID: "review-native", alive: true}
					engine := newEngineForTest(store, store, prAt("head-one"), store, launcher)
					engine.settingsAuthorizer = localReviewerSettingsAuthorization{}
					accounts := &pinnedTestReviewAccount{store: store, defaultID: "account-A"}
					engine.accounts = accounts
					fresh := fixedReviewerCatalog{currentFingerprint: "current-v1", catalog: ports.AgentModelCatalog{InputFingerprint: "current-v1", LastSuccessAt: new(time.Now()), Models: []ports.AgentModelInfo{{ID: "review-a"}, {ID: "review-b"}}}}
					engine.models = fresh
					first, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := store.UpdateReviewRunResult(ctx, first.Run.ID, domain.ReviewRunComplete, domain.VerdictApproved, "", "", false); err != nil {
						t.Fatal(err)
					}
					if entry == "switch" {
						if _, err := engine.SwitchReviewer(ctx, worker.ID, harness, domain.AgentConfig{Model: "review-b"}); err != nil {
							t.Fatal(err)
						}
					} else {
						state, err := store.ReviewerAgentSettings(ctx, first.Run.ReviewID)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := engine.ChangeReviewerAgentSettings(ctx, first.Run.ReviewID, ReviewerSettingsChange{BaseRevision: state.Revision, IdempotencyKey: "pin-b", Patch: domain.AgentSettingsPatch{Model: new(domain.AgentModelSelection("review-b"))}}); err != nil {
							t.Fatal(err)
						}
					}
					before, err := store.ReviewerAgentSettings(ctx, first.Run.ReviewID)
					if err != nil {
						t.Fatal(err)
					}
					saved, _, err := store.GetSession(ctx, worker.ID)
					if err != nil || saved.ReviewerConfig.Model != "review-a" {
						t.Fatalf("setup needs old worker config: %+v, %v", saved.ReviewerConfig, err)
					}
					if stale {
						fallback := fresh
						fallback.catalog.Stale = true
						fallback.catalog.RefreshState = "error"
						fallback.catalog.RefreshError = "discovery unavailable"
						fallback.catalog.LastSuccessAt = new(time.Now().Add(-24 * time.Hour))
						engine.models = fallback
					}
					accounts.defaultID = "account-B"
					engine.prs = prAt("head-two")
					// Inspector's picker already saved the choice; Run review carries no config override.
					second, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{Harness: harness})
					if err != nil || !second.Created || second.Run.AgentSettings == nil || *second.Run.AgentSettings != *before.Settings || launcher.gotSpec.AgentConfig.Model != "review-b" {
						t.Fatalf("Inspector admission lost pin: created=%v run=%+v config=%+v err=%v", second.Created, second.Run, launcher.gotSpec.AgentConfig, err)
					}
					after, err := store.ReviewerAgentSettings(ctx, first.Run.ReviewID)
					if err != nil || after.Revision != before.Revision || *after.Settings != *before.Settings || after.Owner.AccountID != "account-A" {
						t.Fatalf("admission rewrote pin/account: before=%+v after=%+v err=%v", before, after, err)
					}
					if _, err := store.UpdateReviewRunResult(ctx, second.Run.ID, domain.ReviewRunComplete, domain.VerdictApproved, "", "", false); err != nil {
						t.Fatal(err)
					}
					engine.models = fresh
					engine.prs = prAt("head-three")
					explicit, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{Harness: harness, Config: domain.AgentConfig{Model: "review-a"}})
					last, readErr := store.ReviewerAgentSettings(ctx, first.Run.ReviewID)
					if err != nil || readErr != nil || !explicit.Created || last.Settings.Model != "review-a" || last.Revision != before.Revision+1 {
						t.Fatalf("explicit CLI choice was treated as echo: result=%+v state=%+v err=%v/%v", explicit, last, err, readErr)
					}
				})
			}
		}
	}
}

func TestRejectedRestoreCanRecoverWithUnchangedTrigger(t *testing.T) {
	ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
	worker.ReviewerHarness = domain.ReviewerCodex
	worker.ReviewerConfig = domain.AgentConfig{Model: "review-model"}
	seedReviewWorker(t, store, worker)
	engine := newEngineForTest(store, store, prAt("head"), store, &fakeLauncher{handle: "old-pane"})
	engine.accounts = &pinnedTestReviewAccount{store: store, defaultID: "account-A"}
	engine.models = successfulReviewerModelCatalog("review-model")
	first, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cold := newEngineForTest(store, store, prAt("head"), store, &fakeLauncher{handle: "new-pane", agentSessionID: "new-native"})
	coldSequence := 0
	cold.newID = func() string {
		coldSequence++
		return fmt.Sprintf("cold-%d", coldSequence)
	}
	cold.accounts = &pinnedTestReviewAccount{store: store, defaultID: "account-B"}
	cold.models = fixedReviewerCatalog{currentFingerprint: "current-v1", catalog: ports.AgentModelCatalog{InputFingerprint: "current-v1", Models: []ports.AgentModelInfo{{ID: "review-model"}}}}
	if _, err := cold.RestoreReviewer(ctx, worker.ID); !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) {
		t.Fatalf("restore should reject unqualified launch: %v", err)
	}
	runs, err := store.ListReviewRunsBySession(ctx, worker.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != domain.ReviewRunRunning {
		t.Fatalf("rejection changed running pass: %+v, %v", runs, err)
	}
	if _, err := cold.TriggerWithOptions(ctx, worker.ID, TriggerOptions{Config: domain.AgentConfig{Model: "changed-model"}}); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("selection change must remain conflicted: %v", err)
	}
	cold.models = successfulReviewerModelCatalog("review-model")
	retry, err := cold.TriggerWithOptions(ctx, worker.ID, TriggerOptions{})
	if err != nil || !retry.Created || retry.Run.ID == first.Run.ID {
		t.Fatalf("unchanged trigger did not recover: %+v, %v", retry, err)
	}
	runs, err = store.ListReviewRunsBySession(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.ID == first.Run.ID && run.Status != domain.ReviewRunCancelled {
			t.Fatalf("old pass was not reconciled: %+v", run)
		}
	}
}
