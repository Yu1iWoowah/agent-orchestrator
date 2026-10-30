package review

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestLegacyInspectorConfigAdmitsFirstPass(t *testing.T) {
	for _, tc := range []struct {
		harness domain.ReviewerHarness
		config  domain.AgentConfig
	}{
		{domain.ReviewerCodex, domain.AgentConfig{Model: "saved-model"}},
		{domain.ReviewerClaudeCode, domain.AgentConfig{Model: "saved-model"}},
		{domain.ReviewerOpenCode, domain.AgentConfig{Model: "saved-model"}},
		{domain.ReviewerAmp, domain.AgentConfig{Mode: "high"}},
	} {
		harness := tc.harness
		t.Run(string(harness), func(t *testing.T) {
			ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
			worker.ReviewerHarness = harness
			worker.ReviewerConfig = tc.config
			seedReviewWorker(t, store, worker)
			at := time.Now().UTC()
			row := domain.Review{ID: "legacy-owner", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: harness, AgentSessionID: "legacy-native", ReviewerLaunchID: "legacy-launch", CreatedAt: at, UpdatedAt: at}
			if harness == domain.ReviewerAmp {
				row.AgentSessionID = ""
			}
			if err := store.UpsertReview(ctx, row); err != nil {
				t.Fatal(err)
			}
			launcher := &fakeLauncher{handle: "review-pane", agentSessionID: row.AgentSessionID}
			engine := newEngineForTest(store, store, prAt("head"), store, launcher)
			provider, bound := domain.AgentAccountProviderFor(domain.AgentHarness(harness))
			if bound {
				if err := store.BindReviewerAgentAccount(ctx, row.ID, domain.ResolvedAgentAccount{AccountID: "legacy-account", Provider: provider, Source: domain.AgentAccountSourceRole}, at); err != nil {
					t.Fatal(err)
				}
				engine.accounts = &pinnedTestReviewAccount{store: store, defaultID: "new-default-account"}
			}
			engine.models = successfulReviewerModelCatalog("saved-model")
			result, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{Config: worker.ReviewerConfig})
			if err != nil || !result.Created || !launcher.spawned || launcher.gotSpec.AgentSessionID != row.AgentSessionID || launcher.gotSpec.AgentConfig.Mode != worker.ReviewerConfig.Mode {
				t.Fatalf("legacy Inspector intent rejected: result=%+v launch=%+v err=%v", result, launcher, err)
			}
			state, err := store.ReviewerAgentSettings(ctx, row.ID)
			want := reviewerSelectionSettings(worker.ReviewerConfig)
			if bound && state.Owner.AccountID != "legacy-account" {
				t.Fatalf("legacy account changed: %+v", state.Owner)
			}
			if err != nil || state.Settings == nil || *state.Settings != want || state.Revision != 0 || result.Run.AgentSettings == nil || *result.Run.AgentSettings != want {
				t.Fatalf("first admitted pass did not pin saved choice: state=%+v run=%+v err=%v", state, result.Run, err)
			}
		})
	}
}

func TestUnlaunchedOtherReviewerCanRetryDiscovery(t *testing.T) {
	for _, harness := range []domain.ReviewerHarness{domain.ReviewerOpenCode, domain.ReviewerCursor, domain.ReviewerAmp} {
		t.Run(string(harness), func(t *testing.T) {
			ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
			worker.ReviewerHarness = harness
			seedReviewWorker(t, store, worker)
			launcher := &fakeLauncher{handle: "review-pane", agentSessionID: "new-native"}
			engine := newEngineForTest(store, store, prAt("head"), store, launcher)
			engine.models = fixedReviewerCatalog{catalog: ports.AgentModelCatalog{InputFingerprint: "current-v1", RefreshState: "idle", Models: []ports.AgentModelInfo{{ID: "chosen-model"}}}}
			intent := TriggerOptions{Config: domain.AgentConfig{Model: "chosen-model"}}
			if _, err := engine.TriggerWithOptions(ctx, worker.ID, intent); !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) || launcher.spawned {
				t.Fatalf("unqualified initial intent: launch=%+v err=%v", launcher, err)
			}
			row, found, err := store.GetReviewBySessionAndHarness(ctx, worker.ID, harness)
			if err != nil || !found {
				t.Fatalf("initial owner missing: %+v, %v", row, err)
			}
			engine.models = successfulReviewerModelCatalog("chosen-model")
			result, err := engine.TriggerWithOptions(ctx, worker.ID, intent)
			if err != nil || !result.Created || !launcher.spawned || result.Run.ReviewID != row.ID {
				t.Fatalf("qualified initial retry rejected: result=%+v launch=%+v err=%v", result, launcher, err)
			}
		})
	}
}

func TestSwitchReviewerClearsOverrideToResolvedDefault(t *testing.T) {
	for _, tc := range []struct {
		name          string
		defaultAgent  domain.ReviewerHarness
		config        domain.AgentConfig
		activeHarness domain.ReviewerHarness
	}{
		{name: "different default", defaultAgent: domain.ReviewerClaudeCode},
		{name: "default model", defaultAgent: domain.ReviewerClaudeCode, config: domain.AgentConfig{Model: "new-default-model"}},
		{name: "target active", defaultAgent: domain.ReviewerClaudeCode, activeHarness: domain.ReviewerClaudeCode},
		{name: "previous active", defaultAgent: domain.ReviewerClaudeCode, activeHarness: domain.ReviewerCodex},
		{name: "same effective default", defaultAgent: domain.ReviewerCodex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
			worker.ReviewerHarness = domain.ReviewerCodex
			worker.ReviewerConfig = domain.AgentConfig{Model: "codex-model"}
			seedReviewWorker(t, store, worker)
			at := time.Now().UTC()
			for _, harness := range []domain.ReviewerHarness{domain.ReviewerCodex, domain.ReviewerClaudeCode} {
				row := domain.Review{ID: string(harness), SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: harness, AgentSessionID: string(harness) + "-native", ReviewerLaunchID: string(harness) + "-launch", CreatedAt: at, UpdatedAt: at}
				if harness == tc.activeHarness {
					row.ReviewerHandleID = "active-pane"
				}
				if err := store.UpsertReview(ctx, row); err != nil {
					t.Fatal(err)
				}
				provider, _ := domain.AgentAccountProviderFor(domain.AgentHarness(harness))
				if err := store.BindReviewerAgentAccount(ctx, row.ID, domain.ResolvedAgentAccount{AccountID: string(harness) + "-account", Provider: provider, Source: domain.AgentAccountSourceRole}, at); err != nil {
					t.Fatal(err)
				}
				settings := domain.AgentSettings{Model: domain.AgentModelSelection(string(harness) + "-model"), FastMode: domain.AgentFastDefault}
				if _, err := store.InitializeReviewerAgentSettings(ctx, row.ID, settings, at); err != nil {
					t.Fatal(err)
				}
				if harness == tc.activeHarness {
					if err := store.InsertReviewRun(ctx, domain.ReviewRun{ID: "active-pass", ReviewID: row.ID, SessionID: worker.ID, Harness: harness, PRURL: "https://github.com/o/r/pull/1", TargetSHA: "head", Status: domain.ReviewRunRunning, Verdict: domain.VerdictNone, AgentSettings: &settings, CreatedAt: at}); err != nil {
						t.Fatal(err)
					}
				}
			}
			runsBefore, _ := store.ListReviewRunsBySession(ctx, worker.ID)
			oldPin, _ := store.ReviewerAgentSettings(ctx, string(domain.ReviewerCodex))
			launcher := &fakeLauncher{alive: true, handle: "default-pane"}
			engine := newEngineForTest(store, store, fakePRs{}, fakeProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: tc.defaultAgent}}}}, launcher)
			engine.settingsAuthorizer = localReviewerSettingsAuthorization{}
			engine.models = successfulReviewerModelCatalog("new-default-model")
			result, err := engine.SwitchReviewer(ctx, worker.ID, "", tc.config)
			saved, _, readErr := store.GetSession(ctx, worker.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.activeHarness == tc.defaultAgent {
				if !errors.Is(err, ports.ErrAgentSettingsConflict) || saved.ReviewerHarness != worker.ReviewerHarness || launcher.destroyed || launcher.restored {
					t.Fatalf("wrong conflict target: result=%+v saved=%+v err=%v", result, saved, err)
				}
			} else {
				if err != nil || saved.ReviewerHarness != "" || result.ReviewerHarness != tc.defaultAgent {
					t.Fatalf("default selection not persisted/resolved: result=%+v saved=%+v err=%v", result, saved, err)
				}
				if tc.config.Model != "" {
					state, err := store.ReviewerAgentSettings(ctx, string(tc.defaultAgent))
					if err != nil || state.Settings == nil || state.Settings.Model != domain.AgentModelSelection(tc.config.Model) || state.Revision != 1 {
						t.Fatalf("default reviewer choice not pinned: %+v, %v", state, err)
					}
				}
			}
			if tc.defaultAgent == domain.ReviewerCodex {
				row, _, err := store.GetReviewBySessionAndHarness(ctx, worker.ID, domain.ReviewerCodex)
				if err != nil || row.AgentSessionID != "codex-native" {
					t.Fatalf("clearing override lost pinned native history: native=%q err=%v", row.AgentSessionID, err)
				}
				engine.projects = fakeProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerOpenCode}}}}
				selected, _, err := engine.reviewerSelection(ctx, saved)
				if err != nil || selected != domain.ReviewerOpenCode {
					t.Fatalf("cleared override stopped following project defaults: harness=%q err=%v", selected, err)
				}
			}
			oldAfter, _ := store.ReviewerAgentSettings(ctx, string(domain.ReviewerCodex))
			if !reflect.DeepEqual(oldAfter.Settings, oldPin.Settings) || oldAfter.Revision != oldPin.Revision {
				t.Fatalf("changed previous reviewer pin: before=%+v after=%+v", oldPin, oldAfter)
			}
			runsAfter, _ := store.ListReviewRunsBySession(ctx, worker.ID)
			if !reflect.DeepEqual(runsAfter, runsBefore) || tc.activeHarness == domain.ReviewerCodex && launcher.destroyedHandle == "active-pane" {
				t.Fatalf("active pass changed: before=%+v after=%+v launch=%+v", runsBefore, runsAfter, launcher)
			}
		})
	}
}

func TestPinnedRestoreWithoutNativeHistoryRequiresAdmission(t *testing.T) {
	for _, harness := range []domain.ReviewerHarness{domain.ReviewerCodex, domain.ReviewerClaudeCode} {
		for _, preflightFailed := range []bool{true, false} {
			t.Run(string(harness)+"/preflightFailed="+map[bool]string{true: "true", false: "false"}[preflightFailed], func(t *testing.T) {
				ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
				worker.ReviewerHarness = harness
				worker.ReviewerConfig = domain.AgentConfig{Model: "review-model", Effort: "high"}
				seedReviewWorker(t, store, worker)
				launcher := &fakeLauncher{handle: "old-pane"}
				if preflightFailed {
					launcher.preflightErr = errors.New("fake binary unavailable")
				}
				engine := newEngineForTest(store, store, prAt("head"), store, launcher)
				engine.accounts = &pinnedTestReviewAccount{store: store, defaultID: "account-A"}
				engine.models = successfulReviewerModelCatalog("review-model", "high")
				admission, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{})
				wantStatus := domain.ReviewRunRunning
				if preflightFailed {
					wantStatus = domain.ReviewRunFailed
					if err == nil || launcher.spawned {
						t.Fatal("setup must fail at preflight without launching")
					}
				} else if err != nil || !admission.Created || !launcher.spawned {
					t.Fatalf("setup must launch without a native ID: created=%v err=%v", admission.Created, err)
				}
				row, _, err := store.GetReviewBySessionAndHarness(ctx, worker.ID, harness)
				if err != nil || row.AgentSessionID != "" || row.ProviderConversationID != "" {
					t.Fatalf("setup has native history: %+v, %v", row, err)
				}
				before, _ := store.ReviewerAgentSettings(ctx, row.ID)
				runsBefore, _ := store.ListReviewRunsBySession(ctx, worker.ID)
				if before.Settings == nil || len(runsBefore) != 1 || runsBefore[0].Status != wantStatus {
					t.Fatalf("setup must leave a failed pass and admitted pin: %+v %+v", before, runsBefore)
				}
				coldLaunch := &fakeLauncher{handle: "new-pane", agentSessionID: "new-native"}
				cold := newEngineForTest(store, store, prAt("head"), store, coldLaunch)
				cold.accounts = &pinnedTestReviewAccount{store: store, defaultID: "account-B"}
				cold.models = fixedReviewerCatalog{catalog: ports.AgentModelCatalog{InputFingerprint: "current-v1", RefreshState: "idle", WarningCode: ports.ModelCatalogWarningAuthRequired, Models: []ports.AgentModelInfo{{ID: "review-model", Efforts: []string{"high"}}}}}
				result, err := cold.RestoreReviewer(ctx, worker.ID)
				if !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) || result.Restored || coldLaunch.restored {
					t.Fatalf("pinned fresh restore bypassed qualification: result=%+v launch=%+v err=%v", result, coldLaunch, err)
				}
				after, _ := store.ReviewerAgentSettings(ctx, row.ID)
				runsAfter, _ := store.ListReviewRunsBySession(ctx, worker.ID)
				if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(runsBefore, runsAfter) || after.Owner.AccountID != "account-A" {
					t.Fatalf("rejection changed owner/pin/pass: before=%+v after=%+v", before, after)
				}
				cold.models = successfulReviewerModelCatalog("review-model", "high")
				result, err = cold.RestoreReviewer(ctx, worker.ID)
				if err != nil || !result.Restored || !coldLaunch.restored || coldLaunch.gotSpec.PinnedSettings == nil || *coldLaunch.gotSpec.PinnedSettings != *before.Settings {
					t.Fatalf("successful catalog restore lost admitted pin: result=%+v launch=%+v err=%v", result, coldLaunch, err)
				}
				// Once native history exists, its pinned restore remains available without discovery.
				cold.models = nil
				coldLaunch.restored = false
				result, err = cold.RestoreReviewer(ctx, worker.ID)
				if err != nil || !result.Restored || !coldLaunch.restored || coldLaunch.gotSpec.AgentSessionID != "new-native" {
					t.Fatalf("native pinned restore lost compatibility: result=%+v err=%v", result, err)
				}
			})
		}
	}
}
