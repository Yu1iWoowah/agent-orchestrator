package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/amp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/cursor"
	agentregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestReviewerDeclaredFieldsMatrixFailsClosed(t *testing.T) {
	for _, entry := range agentregistry.Harnessed() {
		harness := domain.ReviewerHarness(entry.Harness)
		if !harness.IsKnown() {
			continue
		}
		t.Run(string(harness), func(t *testing.T) {
			spec, err := entry.Agent.GetConfigSpec(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			fields := map[string]bool{}
			for _, field := range spec.Fields {
				fields[field.Key] = true
			}
			engine := &Engine{agents: reviewerDeclaredAgent{agent: entry.Agent}, models: fixedReviewerCatalog{catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), InputFingerprint: "current-v1", Models: []ports.AgentModelInfo{{ID: "model-variant", IsDefault: true, Efforts: []string{"high"}, SupportsFast: true}}}}}
			for _, field := range []string{"model", "effort", "fast"} {
				t.Run(field, func(t *testing.T) {
					config := domain.AgentConfig{}
					wantSupport := fields[field]
					switch field {
					case "model":
						config.Model = "model-variant"
					case "effort":
						config.Effort = "high"
					case "fast":
						config.FastMode = domain.AgentFastOn
						wantSupport = entry.Harness.SupportsFastMode()
					}
					// Typed model does not reinterpret Amp's native mode alias.
					_, err := engine.reviewerModelProof(context.Background(), harness, config, true)
					if wantSupport && err != nil || !wantSupport && !errors.Is(err, ports.ErrAgentSettingsUnsupported) {
						t.Fatalf("%s declared=%v: %v", field, wantSupport, err)
					}
				})
			}
		})
	}
}

type reviewerDeclaredAgent struct{ agent ports.Agent }

func (a reviewerDeclaredAgent) Agent(domain.AgentHarness) (ports.Agent, bool) {
	return a.agent, a.agent != nil
}

type scopedReviewerCatalog struct {
	fixedReviewerCatalog
	accountID string
}

func (m scopedReviewerCatalog) CatalogFingerprint(ctx context.Context, harness, scope string) (string, error) {
	account, ok := ports.AgentAccountFromContext(ctx)
	if !ok || account.AccountID != m.accountID || scope != ports.AccountModelCatalogScope(m.accountID) {
		return "", ports.ErrAgentAccountChanged
	}
	return m.fixedReviewerCatalog.CatalogFingerprint(ctx, harness, scope)
}
func (m scopedReviewerCatalog) ModelCatalog(ctx context.Context, harness, scope string, refresh bool) (ports.AgentModelCatalog, error) {
	if _, err := m.CatalogFingerprint(ctx, harness, scope); err != nil {
		return ports.AgentModelCatalog{}, err
	}
	return m.fixedReviewerCatalog.ModelCatalog(ctx, harness, scope, refresh)
}

func TestReviewerCatalogProofUsesItsBoundAccountScope(t *testing.T) {
	for _, id := range []string{"device-codex", "managed-reviewer-A"} {
		t.Run(id, func(t *testing.T) {
			ctx := ports.WithAgentAccount(context.Background(), domain.ResolvedAgentAccount{AccountID: id, Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceRole})
			engine := &Engine{models: scopedReviewerCatalog{accountID: id, fixedReviewerCatalog: fixedReviewerCatalog{currentFingerprint: "qualified-" + id, catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), InputFingerprint: "qualified-" + id, Models: []ports.AgentModelInfo{{ID: "review-model", SupportsFast: true}}}}}}
			if err := engine.validateReviewerModels(ctx, domain.ReviewerCodex, domain.AgentConfig{Model: "review-model", FastMode: domain.AgentFastOn}); err != nil {
				t.Fatalf("bound account proof failed: %v", err)
			}
		})
	}
}

func TestReviewerRejectsCatalogEffortWithoutAdapterWire(t *testing.T) {
	for _, tc := range []struct {
		harness domain.ReviewerHarness
		adapter ports.Agent
	}{{domain.ReviewerCursor, cursor.New()}, {domain.ReviewerAmp, amp.New()}} {
		t.Run(string(tc.harness), func(t *testing.T) {
			engine := &Engine{models: fixedReviewerCatalog{catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), InputFingerprint: "current-v1", Models: []ports.AgentModelInfo{{ID: "native-variant", Efforts: []string{"high"}}}}}}
			// The production resolver supplies the adapter's declared field spec.
			engine.agents = reviewerDeclaredAgent{agent: tc.adapter}
			err := engine.validateReviewerModels(context.Background(), tc.harness, domain.AgentConfig{Model: "native-variant", Effort: "high"})
			if !errors.Is(err, ports.ErrAgentSettingsUnsupported) {
				t.Fatalf("variant/mode became generic effort: %v", err)
			}
		})
	}
}

type localReviewerSettingsAuthorization struct{}

func TestSwitchReviewerCannotChangeAnActivePass(t *testing.T) {
	worker := liveWorker()
	worker.ReviewerHarness = domain.ReviewerCodex
	store := &fakeStore{review: &domain.Review{ID: "review-owner", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerCodex, ReviewerHandleID: "live-pane"}, runs: []domain.ReviewRun{{ID: "active-pass", ReviewID: "review-owner", SessionID: worker.ID, Harness: domain.ReviewerCodex, Status: domain.ReviewRunRunning}}}
	launcher := &fakeLauncher{alive: true}
	engine := newEngineForTest(store, fakeSessions{rec: worker, ok: true}, prAt("head"), fakeProjects{}, launcher)
	_, err := engine.SwitchReviewer(context.Background(), worker.ID, domain.ReviewerCodex, domain.AgentConfig{Model: "another-model"})
	if !errors.Is(err, ports.ErrAgentSettingsConflict) || len(store.reviewerConfigUpdates) != 0 || launcher.destroyed || launcher.spawned || launcher.restored || launcher.cancelled {
		t.Fatalf("active reviewer changed: err=%v updates=%+v launcher=%+v", err, store.reviewerConfigUpdates, launcher)
	}
}

func (localReviewerSettingsAuthorization) AuthorizeReviewerSelection(context.Context, domain.SessionRecord, domain.Review, domain.AgentSettings) (ports.ReviewerSettingsAuthorization, error) {
	return ports.ReviewerSettingsAuthorization{Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}, PolicyRevision: "local_operator_unverified"}, nil
}

type pinnedTestReviewAccount struct {
	ports.ReviewerAccountBindings
	store     *sqlite.Store
	defaultID string
}

func (a *pinnedTestReviewAccount) ResolveReviewerAccount(ctx context.Context, id string, req ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	p, _ := domain.AgentAccountProviderFor(req.Harness)
	bound, found, err := a.store.GetReviewerAgentAccount(ctx, id, p)
	if err != nil || found {
		return bound, err
	}
	bound = domain.ResolvedAgentAccount{AccountID: a.defaultID, Provider: p, Source: domain.AgentAccountSourceRole}
	return bound, a.store.BindReviewerAgentAccount(ctx, id, bound, time.Now())
}

func TestReviewerNextPassPinsItsOwnSelectionAndAccount(t *testing.T) {
	ctx := context.Background()
	store, worker := newSQLiteReviewStore(t), liveWorker()
	worker.ReviewerHarness = domain.ReviewerCodex
	worker.ReviewerConfig = domain.AgentConfig{Model: "review-one", Effort: "low", FastMode: domain.AgentFastOff}
	seedReviewWorker(t, store, worker)
	launcher := &fakeLauncher{handle: "review-pane", agentSessionID: "review-native"}
	engine := newEngineForTest(store, store, prAt("head-one"), store, launcher)
	engine.settingsAuthorizer = localReviewerSettingsAuthorization{}
	accounts := &pinnedTestReviewAccount{store: store, defaultID: "account-A"}
	engine.accounts = accounts
	engine.models = fixedReviewerCatalog{currentFingerprint: "same-input", catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), InputFingerprint: "same-input", Models: []ports.AgentModelInfo{{ID: "review-one", Efforts: []string{"low"}, SupportsFast: true}, {ID: "review-two", Efforts: []string{"high"}, SupportsFast: true}}}}
	first, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.ReviewerAgentSettings(ctx, first.Run.ReviewID)
	if err != nil || state.Settings == nil || state.Settings.Model != "review-one" {
		t.Fatalf("reviewer initial selection = %+v, %v", state, err)
	}
	req := ReviewerSettingsChange{BaseRevision: state.Revision, IdempotencyKey: "next-pass", Patch: domain.AgentSettingsPatch{Model: new(domain.AgentModelSelection("review-two")), Effort: new(domain.AgentEffortSelection("high")), FastMode: new(domain.AgentFastOn)}}
	if _, err = engine.ChangeReviewerAgentSettings(ctx, first.Run.ReviewID, req); !errors.Is(err, ports.ErrAgentSettingsConflict) || launcher.destroyed || launcher.cancelled {
		t.Fatalf("active pass = %v, destroyed=%v, cancelled=%v", err, launcher.destroyed, launcher.cancelled)
	}
	if _, err = engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{Config: domain.AgentConfig{Model: "review-two"}}); !errors.Is(err, ports.ErrAgentSettingsConflict) || launcher.destroyed || launcher.cancelled {
		t.Fatalf("active legacy trigger = %v, destroyed=%v", err, launcher.destroyed)
	}
	if _, err = store.UpdateReviewRunResult(ctx, first.Run.ID, domain.ReviewRunComplete, domain.VerdictApproved, "", "", false); err != nil {
		t.Fatal(err)
	}
	op, err := engine.ChangeReviewerAgentSettings(ctx, first.Run.ReviewID, req)
	if err != nil || op.Revision != 1 || op.Phase != "pinned" {
		t.Fatalf("next pass pin = %+v, %v", op, err)
	}
	accounts.defaultID = "account-B"
	engine.prs = prAt("head-two")
	second, err := engine.TriggerWithOptions(ctx, worker.ID, TriggerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Run.AgentSettings == nil || *second.Run.AgentSettings != op.Target || launcher.gotSpec.PinnedSettings == nil || *launcher.gotSpec.PinnedSettings != op.Target || second.Run.ReviewID != first.Run.ReviewID || launcher.gotSpec.AgentConfig.Model != "review-two" || launcher.gotSpec.AgentConfig.FastMode != domain.AgentFastOn {
		t.Fatalf("next pass = %+v, launch=%+v", second.Run, launcher.gotSpec)
	}
	bound, found, err := store.GetReviewerAgentAccount(ctx, first.Run.ReviewID, domain.AgentAccountProviderCodex)
	if err != nil || !found || bound.AccountID != "account-A" {
		t.Fatalf("pass reselected account: %+v %v", bound, err)
	}
	workerAfter, _, err := store.GetSession(ctx, worker.ID)
	if err != nil || workerAfter.Metadata.Model != worker.Metadata.Model || workerAfter.ReviewerConfig != worker.ReviewerConfig {
		t.Fatalf("worker changed: %+v, %v", workerAfter, err)
	}
	launcher.restored = false
	launcher.spawned = false
	if _, err = engine.RestoreReviewer(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	if !launcher.restored || launcher.gotSpec.PinnedSettings == nil || *launcher.gotSpec.PinnedSettings != op.Target || launcher.gotSpec.AgentConfig.Model != "review-two" || launcher.gotSpec.AgentConfig.FastMode != domain.AgentFastOn || launcher.gotSpec.AgentSessionID != "review-native" {
		t.Fatalf("cold pass restore changed pinned settings/history: %+v", launcher.gotSpec)
	}
}

func TestRequestedReviewerFastModeReachesItsLauncher(t *testing.T) {
	for _, mode := range []domain.AgentFastMode{domain.AgentFastOn, domain.AgentFastOff} {
		t.Run(string(mode), func(t *testing.T) {
			store := &fakeStore{}
			worker := liveWorker()
			worker.ReviewerHarness = domain.ReviewerCodex
			worker.ReviewerConfig = domain.AgentConfig{Model: "previous-reviewer-model"}
			launcher := &fakeLauncher{handle: "reviewer-pane"}
			engine := newEngineForTest(store, fakeSessions{rec: worker, ok: true}, prAt("head-1"), fakeProjects{}, launcher)
			engine.models = fixedReviewerCatalog{catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), InputFingerprint: "current-v1", Models: []ports.AgentModelInfo{{ID: "requested-reviewer-model", SupportsFast: true}}}}
			_, err := trigger(context.Background(), engine, worker.ID, domain.ReviewerCodex, domain.AgentConfig{Model: "requested-reviewer-model", FastMode: mode})
			if err != nil {
				t.Fatal(err)
			}
			if got := launcher.gotSpec.AgentConfig; got.Model != "requested-reviewer-model" || got.FastMode != mode {
				t.Fatalf("launched reviewer settings = %+v; want requested-reviewer-model, fast=%q", got, mode)
			}
			if len(store.reviewerConfigUpdates) != 0 {
				t.Fatal("a one-pass request rewrote the worker's reviewer defaults")
			}
		})
	}
}

type fixedReviewerCatalog struct {
	catalog            ports.AgentModelCatalog
	currentFingerprint string
}

func (m fixedReviewerCatalog) ModelCatalog(context.Context, string, string, bool) (ports.AgentModelCatalog, error) {
	return m.catalog, nil
}

func (m fixedReviewerCatalog) CatalogFingerprint(ctx context.Context, _, _ string) (string, error) {
	if m.currentFingerprint != "" {
		return m.currentFingerprint, nil
	}
	if account, ok := ports.AgentAccountFromContext(ctx); ok {
		return account.AccountID, nil
	}
	return "current-v1", nil
}

func TestReviewerFastRequiresQualifiedModelEvidence(t *testing.T) {
	fresh := ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), Models: []ports.AgentModelInfo{{ID: "review-model", IsDefault: true, SupportsFast: true}}}
	for _, tc := range []struct {
		name    string
		harness domain.ReviewerHarness
		mode    domain.AgentFastMode
		catalog ports.AgentModelCatalog
		want    error
	}{
		{name: "supported on", mode: domain.AgentFastOn, catalog: fresh},
		{name: "supported off", mode: domain.AgentFastOff, catalog: fresh},
		{name: "no fast on", mode: domain.AgentFastOn, catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), Models: []ports.AgentModelInfo{{ID: "review-model"}}}, want: ports.ErrAgentSettingsUnsupported},
		{name: "off remains standard", mode: domain.AgentFastOff, catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), Models: []ports.AgentModelInfo{{ID: "review-model"}}}},
		{name: "stale after refresh", mode: domain.AgentFastOn, catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), Stale: true, Models: fresh.Models}, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "refresh failure", mode: domain.AgentFastOn, catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), RefreshError: "discovery unavailable", Models: fresh.Models}, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "queued", mode: domain.AgentFastOn, catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), RefreshState: "queued", Models: fresh.Models}, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "refreshing", mode: domain.AgentFastOn, catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), RefreshState: "refreshing", Models: fresh.Models}, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "unknown custom model", mode: domain.AgentFastOn, catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), AllowCustom: true}, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "Amp mode is not fast", harness: domain.ReviewerAmp, mode: domain.AgentFastOn, catalog: fresh, want: ports.ErrAgentSettingsUnsupported},
		{name: "Amp has no fast off", harness: domain.ReviewerAmp, mode: domain.AgentFastOff, catalog: fresh, want: ports.ErrAgentSettingsUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := tc.harness
			if harness == "" {
				harness = domain.ReviewerCodex
			}
			tc.catalog.InputFingerprint = "current-v1"
			engine := &Engine{models: fixedReviewerCatalog{catalog: tc.catalog}}
			err := engine.validateReviewerModels(context.Background(), harness, domain.AgentConfig{Model: "review-model", FastMode: tc.mode})
			if !errors.Is(err, tc.want) {
				t.Fatalf("validation = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("missing discovery cannot qualify fast", func(t *testing.T) {
		engine := &Engine{}
		if err := engine.validateReviewerModels(context.Background(), domain.ReviewerCodex, domain.AgentConfig{FastMode: domain.AgentFastOn}); !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) {
			t.Fatalf("validation = %v, want capabilities unavailable", err)
		}
	})
}

func TestReviewerFastRejectsChangedDiscoveryInputsBeforeRefresh(t *testing.T) {
	for _, input := range []string{"binary-v2", "account-B"} {
		t.Run(input, func(t *testing.T) {
			catalog := ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), InputFingerprint: "old-input", Models: []ports.AgentModelInfo{{ID: "review-model", SupportsFast: true}}}
			engine := &Engine{models: fixedReviewerCatalog{catalog: catalog, currentFingerprint: input}}
			ctx := ports.WithAgentAccount(context.Background(), domain.ResolvedAgentAccount{AccountID: "account-B", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceRole})
			if err := engine.validateReviewerModels(ctx, domain.ReviewerCodex, domain.AgentConfig{Model: "review-model", FastMode: domain.AgentFastOn}); !errors.Is(err, ports.ErrAgentSettingsCapabilitiesUnavailable) {
				t.Fatalf("validation = %v, want capabilities unavailable for changed %s", err, input)
			}
		})
	}
}

func TestSwitchExistingReviewerWithoutOverridePreservesItsPreference(t *testing.T) {
	ctx, store, worker := context.Background(), newSQLiteReviewStore(t), liveWorker()
	worker.ReviewerHarness = domain.ReviewerCodex
	worker.ReviewerConfig = domain.AgentConfig{Model: "codex-worker-default"}
	seedReviewWorker(t, store, worker)
	at := time.Now().UTC()
	row := domain.Review{ID: "existing-claude", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerClaudeCode, AgentSessionID: "claude-native", ReviewerLaunchID: "claude-launch", CreatedAt: at, UpdatedAt: at}
	if err := store.UpsertReview(ctx, row); err != nil {
		t.Fatal(err)
	}
	own := domain.AgentSettings{Model: "claude-own-model", Effort: "high", FastMode: domain.AgentFastOn}
	if _, err := store.InitializeReviewerAgentSettings(ctx, row.ID, own, at); err != nil {
		t.Fatal(err)
	}
	account := domain.ResolvedAgentAccount{AccountID: "claude-account-A", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole}
	if err := store.BindReviewerAgentAccount(ctx, row.ID, account, at); err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLauncher{handle: "claude-pane", agentSessionID: "claude-native"}
	engine := newEngineForTest(store, store, fakePRs{}, store, launcher)
	// No settings authorizer: selecting an existing reviewer is not a mutation.
	if _, err := engine.SwitchReviewer(ctx, worker.ID, domain.ReviewerClaudeCode, domain.AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReviewerAgentSettings(ctx, row.ID)
	if err != nil || state.Settings == nil || *state.Settings != own || state.Revision != 0 || state.Owner.AccountID != account.AccountID {
		t.Fatalf("reviewer preference changed: %+v, %v", state, err)
	}
	if launcher.gotSpec.AgentConfig.Model != string(own.Model) || launcher.gotSpec.AgentConfig.Effort != string(own.Effort) || launcher.gotSpec.AgentConfig.FastMode != own.FastMode {
		t.Fatalf("restored another reviewer's defaults: %+v", launcher.gotSpec)
	}
}
