package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReviewerStrictCatalogNeedsLastSuccessfulEvidence(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		last *time.Time
		want error
	}{
		{"never succeeded", nil, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"zero success", new(time.Time{}), ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"future success", new(now.Add(time.Hour)), ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"previous day", new(now.Add(-24 * time.Hour)), ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"current success", &now, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := ports.AgentModelCatalog{InputFingerprint: "current-v1", FetchedAt: now, ValidatedAt: now, LastSuccessAt: tc.last, RefreshState: "idle", Models: []ports.AgentModelInfo{{ID: "review-model", IsDefault: true, SupportsFast: true}}}
			e := &Engine{models: fixedReviewerCatalog{catalog: catalog}}
			_, err := e.reviewerModelProof(context.Background(), domain.ReviewerCodex, domain.AgentConfig{Model: "review-model", FastMode: domain.AgentFastOn}, reviewerCatalogMutation)
			if !errors.Is(err, tc.want) {
				t.Fatalf("unqualified catalog accepted: %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReviewerAdmissionWithoutRefreshFailureNeedsFreshEvidence(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		last *time.Time
		want error
	}{
		{"never succeeded", nil, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"zero success", new(time.Time{}), ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"future success", new(now.Add(time.Hour)), ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"previous day", new(now.Add(-24 * time.Hour)), ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"current success", &now, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := ports.AgentModelCatalog{InputFingerprint: "current-v1", FetchedAt: now, ValidatedAt: now, LastSuccessAt: tc.last, RefreshState: "idle", Models: []ports.AgentModelInfo{{ID: "review-model", IsDefault: true, Efforts: []string{"high"}}}}
			e := &Engine{models: fixedReviewerCatalog{catalog: catalog}}
			err := e.validateReviewerModels(context.Background(), domain.ReviewerCodex, domain.AgentConfig{Model: "review-model", Effort: "high", FastMode: domain.AgentFastDefault})
			if !errors.Is(err, tc.want) {
				t.Fatalf("admission evidence: %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReviewerAdmissionWithoutCatalogDoesNotQualifySelections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config domain.AgentConfig
		want   error
	}{
		{"model", domain.AgentConfig{Model: "review-model"}, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"effort", domain.AgentConfig{Effort: "high"}, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"fast", domain.AgentConfig{FastMode: domain.AgentFastOn}, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"provider defaults without catalog selection", domain.AgentConfig{FastMode: domain.AgentFastDefault}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{}
			if err := e.validateReviewerModels(context.Background(), domain.ReviewerCodex, tc.config); !errors.Is(err, tc.want) {
				t.Fatalf("missing admission catalog: %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReviewerCatalogUsesOwnerCalendarDayRule(t *testing.T) {
	zone := time.FixedZone("Taipei", 8*60*60)
	for _, tc := range []struct {
		name      string
		now, last time.Time
		stale     bool
	}{
		{"same day beyond six hours", time.Date(2026, 10, 31, 18, 0, 0, 0, zone), time.Date(2026, 10, 31, 9, 0, 0, 0, zone), false},
		{"same instant across zones", time.Date(2026, 10, 31, 0, 10, 0, 0, zone), time.Date(2026, 10, 30, 16, 5, 0, 0, time.UTC), false},
		{"previous local day", time.Date(2026, 10, 31, 0, 10, 0, 0, zone), time.Date(2026, 10, 30, 15, 55, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := ports.AgentModelCatalog{LastSuccessAt: &tc.last, Models: []ports.AgentModelInfo{{ID: "review-model", SupportsFast: true}}}
			err := validateReviewerCatalog(domain.ReviewerCodex, catalog, domain.AgentConfig{Model: "review-model", FastMode: domain.AgentFastOn}, true, tc.now)
			if (err != nil) != tc.stale {
				t.Fatalf("calendar evidence: %v, stale=%v", err, tc.stale)
			}
		})
	}
}

func TestNewReviewerModelEffortRejectsCacheWithoutSuccessfulDiscovery(t *testing.T) {
	for _, harness := range []domain.ReviewerHarness{domain.ReviewerCodex, domain.ReviewerClaudeCode} {
		for _, selection := range []struct {
			name   string
			config domain.AgentConfig
		}{
			{"model", domain.AgentConfig{Model: "review-model", FastMode: domain.AgentFastDefault}},
			{"effort", domain.AgentConfig{Effort: "high", FastMode: domain.AgentFastDefault}},
			{"model and effort", domain.AgentConfig{Model: "review-model", Effort: "high", FastMode: domain.AgentFastDefault}},
		} {
			t.Run(string(harness)+"/"+selection.name, func(t *testing.T) {
				ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
				worker.ReviewerHarness = harness
				seedReviewWorker(t, store, worker)
				catalog := ports.AgentModelCatalog{InputFingerprint: "current-v1", FetchedAt: time.Now(), RefreshState: "idle", WarningCode: ports.ModelCatalogWarningAuthRequired, Models: []ports.AgentModelInfo{{ID: "review-model", IsDefault: true, Efforts: []string{"high"}}}}
				launcher := &fakeLauncher{handle: "review-pane", agentSessionID: "review-native"}
				engine := newEngineForTest(store, store, prAt("head"), store, launcher)
				engine.accounts = &pinnedTestReviewAccount{store: store, defaultID: "reviewer-A"}
				engine.models = fixedReviewerCatalog{currentFingerprint: "current-v1", catalog: catalog}
				result, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{Config: selection.config})
				if !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) || result.Created || launcher.spawned || launcher.restored || launcher.notified {
					t.Fatalf("unqualified new reviewer: created=%v spawned=%v restored=%v err=%v", result.Created, launcher.spawned, launcher.restored, err)
				}
				runs, err := store.ListReviewRunsBySession(ctx, worker.ID)
				if err != nil || len(runs) != 0 {
					t.Fatalf("unqualified pass snapshot persisted: %+v, %v", runs, err)
				}
				row, found, err := store.GetReviewBySessionAndHarness(ctx, worker.ID, harness)
				if err != nil {
					t.Fatal(err)
				}
				if found {
					state, err := store.ReviewerAgentSettings(ctx, row.ID)
					if err != nil || state.Settings != nil {
						t.Fatalf("unqualified preference initialized: %+v, %v", state, err)
					}
				}
				engine.models = successfulReviewerModelCatalog("review-model", "high")
				retry, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{Config: selection.config})
				if err != nil || !retry.Created || !launcher.spawned {
					t.Fatalf("qualified same-intent retry: result=%+v spawned=%v err=%v", retry, launcher.spawned, err)
				}
				state, err := store.ReviewerAgentSettings(ctx, row.ID)
				want := reviewerSelectionSettings(selection.config)
				if err != nil || state.Settings == nil || *state.Settings != want || retry.Run.AgentSettings == nil || *retry.Run.AgentSettings != want {
					t.Fatalf("retry did not pin the original intent: state=%+v run=%+v err=%v", state, retry.Run, err)
				}
			})
		}
	}
}

func TestLegacyReviewerRestoreDoesNotQualifyANewPass(t *testing.T) {
	ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
	worker.ReviewerHarness = domain.ReviewerCodex
	worker.ReviewerConfig = domain.AgentConfig{Model: "legacy-model", Effort: "high"}
	seedReviewWorker(t, store, worker)
	at := time.Now().UTC()
	row := domain.Review{ID: "legacy-owner", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: worker.ReviewerHarness, AgentSessionID: "legacy-native", ReviewerLaunchID: "legacy-launch", CreatedAt: at, UpdatedAt: at}
	if err := store.UpsertReview(ctx, row); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{handle: "legacy-pane", agentSessionID: row.AgentSessionID}
	engine := newEngineForTest(store, store, prAt("head"), store, launcher)
	engine.models = fixedReviewerCatalog{catalog: ports.AgentModelCatalog{InputFingerprint: "current-v1", RefreshState: "idle", WarningCode: ports.ModelCatalogWarningAuthRequired, Models: []ports.AgentModelInfo{{ID: "legacy-model", Efforts: []string{"high"}}}}}
	result, err := engine.RestoreReviewer(ctx, worker.ID)
	if err != nil || !result.Restored || !launcher.restored || launcher.gotSpec.AgentSessionID != row.AgentSessionID || launcher.gotSpec.AgentConfig.Model != worker.ReviewerConfig.Model {
		t.Fatalf("legacy restore changed: result=%+v launch=%+v err=%v", result, launcher, err)
	}
	state, err := store.ReviewerAgentSettings(ctx, row.ID)
	if err != nil || state.Settings != nil {
		t.Fatalf("legacy restore backfilled a pin: %+v, %v", state, err)
	}
	launcher.spawned, launcher.restored = false, false
	resultPass, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{})
	if !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) || resultPass.Created || launcher.spawned || launcher.restored {
		t.Fatalf("restore evidence qualified a new pass: result=%+v launch=%+v err=%v", resultPass, launcher, err)
	}
}

func TestReviewerRestoreWithoutNativeHistoryRequiresSuccessfulCatalog(t *testing.T) {
	for _, harness := range []domain.ReviewerHarness{domain.ReviewerCodex, domain.ReviewerClaudeCode} {
		for _, chatFallback := range []bool{false, true} {
			t.Run(string(harness)+"/chatFallback="+map[bool]string{false: "false", true: "true"}[chatFallback], func(t *testing.T) {
				ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
				worker.ReviewerHarness = harness
				worker.ReviewerConfig = domain.AgentConfig{Model: "review-model", Effort: "high"}
				seedReviewWorker(t, store, worker)
				at := time.Now().UTC()
				row := domain.Review{ID: "unlaunched-owner", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: harness, CreatedAt: at, UpdatedAt: at}
				if chatFallback {
					row.InterfaceMode, row.AgentSessionID = domain.ReviewerInterfaceChat, "unavailable-chat-native"
				}
				if err := store.UpsertReview(ctx, row); err != nil {
					t.Fatal(err)
				}
				launcher := &fakeLauncher{handle: "review-pane", agentSessionID: "new-native"}
				engine := newEngineForTest(store, store, prAt("head"), store, launcher)
				engine.models = fixedReviewerCatalog{catalog: ports.AgentModelCatalog{InputFingerprint: "current-v1", RefreshState: "idle", WarningCode: ports.ModelCatalogWarningAuthRequired, Models: []ports.AgentModelInfo{{ID: "review-model", Efforts: []string{"high"}}}}}
				result, err := engine.RestoreReviewer(ctx, worker.ID)
				if !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) || result.Restored || launcher.restored || launcher.spawned {
					t.Fatalf("unqualified fresh restore: result=%+v restored=%v err=%v", result, launcher.restored, err)
				}
				engine.models = successfulReviewerModelCatalog("review-model", "high")
				result, err = engine.RestoreReviewer(ctx, worker.ID)
				if err != nil || !result.Restored || !launcher.restored {
					t.Fatalf("qualified restore: result=%+v err=%v", result, err)
				}
				if launcher.gotSpec.AgentSessionID != "" {
					t.Fatalf("fresh launch inherited unavailable history: %+v", launcher.gotSpec)
				}
				state, err := store.ReviewerAgentSettings(ctx, row.ID)
				if err != nil || state.Settings != nil {
					t.Fatalf("restore initialized preference: %+v, %v", state, err)
				}
			})
		}
	}
}

func TestLegacyReviewerOverrideStillRequiresRecordedPreference(t *testing.T) {
	for _, evidence := range []string{"handle", "launch", "agent session", "provider conversation", "pass"} {
		t.Run(evidence, func(t *testing.T) {
			ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
			worker.ReviewerHarness = domain.ReviewerCodex
			seedReviewWorker(t, store, worker)
			at := time.Now().UTC()
			row := domain.Review{ID: "legacy-owner", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: worker.ReviewerHarness, CreatedAt: at, UpdatedAt: at}
			switch evidence {
			case "handle":
				row.ReviewerHandleID = "legacy-pane"
			case "launch":
				row.ReviewerLaunchID = "legacy-launch"
			case "agent session":
				row.AgentSessionID = "legacy-native"
			case "provider conversation":
				row.ProviderConversationID = "legacy-conversation"
			}
			if err := store.UpsertReview(ctx, row); err != nil {
				t.Fatal(err)
			}
			if evidence == "pass" {
				if err := store.InsertReviewRun(ctx, domain.ReviewRun{ID: "legacy-pass", ReviewID: row.ID, SessionID: worker.ID, Harness: row.Harness, PRURL: "https://github.com/o/r/pull/1", TargetSHA: "old-head", Status: domain.ReviewRunFailed, Verdict: domain.VerdictNone, CreatedAt: at}); err != nil {
					t.Fatal(err)
				}
			}
			launcher := &fakeLauncher{}
			engine := newEngineForTest(store, store, prAt("head"), store, launcher)
			engine.models = successfulReviewerModelCatalog("new-model")
			result, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{Config: domain.AgentConfig{Model: "new-model"}})
			if !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) || result.Created || launcher.spawned || launcher.restored || launcher.destroyed {
				t.Fatalf("legacy override changed: result=%+v launcher=%+v err=%v", result, launcher, err)
			}
		})
	}
}
