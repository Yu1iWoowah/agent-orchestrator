package sessionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type tuiSettingsAgent struct {
	transitionAgent
	restores   []ports.RestoreConfig
	history    bool
	protocol   string
	onPrepare  func(context.Context) error
	baseline   domain.AgentSettingsNativeValues
	lastSource domain.AgentSettings
}

func (a *tuiSettingsAgent) NativeConversationExists(context.Context, ports.SessionRef, string, map[string]string) (bool, error) {
	return a.history, nil
}
func (a *tuiSettingsAgent) GetRestoreCommand(ctx context.Context, cfg ports.RestoreConfig) ([]string, bool, error) {
	a.restores = append(a.restores, cfg)
	return a.transitionAgent.GetRestoreCommand(ctx, cfg)
}

type tuiSettingsFixture struct {
	m         *Manager
	control   *agentsettings.Control
	store     *sqlite.Store
	rec       domain.SessionRecord
	runtime   *fakeRuntime
	agent     *tuiSettingsAgent
	lifecycle *lifecycle.Manager
}

func newTUISettingsFixture(t *testing.T, h domain.AgentHarness) tuiSettingsFixture {
	t.Helper()
	ctx := t.Context()
	now := time.Now()
	dir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dir)
	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: dir, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Harness: h, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{WorkspacePath: dir, Model: "native", RuntimeHandleID: "source-runtime", RuntimeLaunchID: "source-generation", AgentSessionID: "native-thread", AgentSessionIDLaunchID: "source-generation"}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	provider, _ := domain.AgentAccountProviderFor(h)
	if err := st.BindSessionAgentAccount(ctx, rec.ID, provider, "device-"+string(provider), now); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"agentId": string(h), "models": []map[string]any{{"id": "native", "isDefault": true, "efforts": []string{"low", "high"}, "supportsFast": true}}})
	if err := st.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: string(h), CatalogJSON: string(raw), LastSuccessAt: now, FetchedAt: now, InputFingerprint: "current"}); err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{aliveByHandle: map[string]bool{"source-runtime": true}}
	agent := &tuiSettingsAgent{history: true, protocol: "codex-native-resume"}
	if h == domain.HarnessClaudeCode {
		agent.protocol = "claude-native-resume"
	}
	lc := lifecycle.New(st, nil)
	m := New(Deps{Store: st, Lifecycle: lc, Runtime: rt, Agents: singleAgent{agent: agent}, Workspace: &fakeWorkspace{path: dir}, Messenger: &fakeMessenger{}, DataDir: dir, LookPath: func(string) (string, error) { return "/bin/true", nil }, Executable: func() (string, error) { return filepath.Join(dir, "bin", "ao"), nil }, Logger: slog.New(slog.DiscardHandler)})
	lc.SetSessionOperationGate(m)
	useFastInterfaceTransitionTimings(m)
	core := agentsettings.New(st, func() time.Time { return now })
	core.SetCatalogFingerprintSource(&spawnCatalogFingerprint{current: "current"})
	control := agentsettings.NewControl(core, st, agentsettings.NewAuthorizer(st, nil, func() time.Time { return now }), nil, func() time.Time { return now })
	runtime, ok := any(m).(agentsettings.TUISettingsRuntime)
	if !ok {
		t.Fatal("session manager has no TUI same-conversation settings scheduler")
	}
	control.SetTUIRuntime(runtime)
	setter, ok := any(m).(interface {
		SetTUIAgentSettingsValidator(ports.ChatAgentSettingsValidator)
	})
	if !ok {
		t.Fatal("TUI scheduler has no shared validator boundary")
	}
	setter.SetTUIAgentSettingsValidator(control)
	return tuiSettingsFixture{m, control, st, rec, rt, agent, lc}
}
func (f tuiSettingsFixture) wait(t *testing.T, id string) domain.AgentSettingsOperation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if op.Phase != domain.AgentSettingsPending {
			f.m.transitionMu.Lock()
			run := f.m.transitions[f.rec.ID]
			f.m.transitionMu.Unlock()
			if run != nil {
				select {
				case <-run.done:
				case <-time.After(5 * time.Second):
					t.Fatal("settings worker did not stop")
				}
			}
			latest, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			return latest
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("TUI settings operation did not settle")
	return domain.AgentSettingsOperation{}
}
func TestTUISettingsNativeRestartPreservesConversationAndAccount(t *testing.T) {
	for _, h := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		t.Run(string(h), func(t *testing.T) {
			f := newTUISettingsFixture(t, h)
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			on := domain.AgentFastOn
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high, FastMode: &on}, IdempotencyKey: "restart"})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsApplied {
				t.Fatalf("restart=%+v", op)
			}
			current, err := f.store.SessionAgentSettings(t.Context(), f.rec.ID)
			if err != nil || current.Revision != 1 || current.Settings.Effort != high || current.Settings.FastMode != on || current.ProviderConversationID != "native-thread" {
				t.Fatalf("committed=%+v %v", current, err)
			}
			if f.runtime.created != 1 || f.runtime.destroyed != 1 || len(f.runtime.interrupts) != 0 {
				t.Fatalf("runtime create=%d destroy=%d interrupts=%v", f.runtime.created, f.runtime.destroyed, f.runtime.interrupts)
			}
			last := f.agent.restores[len(f.agent.restores)-1]
			if last.Config.Effort != "high" || !last.Config.Launch.FastMode || last.Session.Metadata[ports.MetadataKeyAgentSessionID] != "native-thread" {
				t.Fatalf("resume config=%+v", last)
			}
		})
	}
}

func TestTUISettingsUnstartedSessionSavesWithoutLaunching(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	rec, err := f.store.CreateSession(t.Context(), domain.SessionRecord{ProjectID: f.rec.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{WorkspacePath: f.rec.Metadata.WorkspacePath}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.BindSessionAgentAccount(t.Context(), rec.ID, domain.AgentAccountProviderCodex, "device-codex", time.Now()); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "save-before-launch"})
	if err != nil || op.Phase != domain.AgentSettingsApplied {
		t.Fatalf("unstarted preference was not saved: %+v %v", op, err)
	}
	current, err := f.store.SessionAgentSettings(t.Context(), rec.ID)
	if err != nil || current.Revision != 1 || current.Settings.Effort != high || current.ProviderConversationID != "" {
		t.Fatalf("unstarted save invented native state: %+v %v", current, err)
	}
	if f.runtime.created != 0 || f.runtime.destroyed != 0 || len(f.runtime.interrupts) != 0 {
		t.Fatal("saving an unstarted preference touched a runtime")
	}
}

func TestTUISettingsUncertainStopKeepsInputFenced(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	f.runtime.destroyErr = errors.New("unconfirmed source shutdown")
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "uncertain"})
	if err != nil {
		t.Fatal(err)
	}
	op = f.wait(t, op.ID)
	if op.Phase != domain.AgentSettingsRecoveryRequired || f.runtime.created != 0 || len(f.runtime.interrupts) != 0 {
		t.Fatalf("uncertain stop launched target or lost recovery: %+v creates=%d interrupts=%v", op, f.runtime.created, f.runtime.interrupts)
	}
	release, admitted := f.m.AcquireSessionInput(f.rec.ID)
	if admitted {
		release()
		t.Fatal("recovery released terminal input fence while source liveness is uncertain")
	}
	current, err := f.store.SessionAgentSettings(t.Context(), f.rec.ID)
	if err != nil || current.Revision != 0 || current.Settings.Effort != "" {
		t.Fatalf("uncertain stop committed: %+v %v", current, err)
	}
}

func TestTUISettingsDaemonCrashDoesNotAutoLaunchOrUnfence(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			current, err := f.store.SessionAgentSettings(t.Context(), f.rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			op, err := f.store.BeginAgentSettingsOperation(t.Context(), domain.AgentSettingsOperation{ID: "crash", SessionID: f.rec.ID, IdempotencyKey: "crash", Source: current.Settings, Target: domain.AgentSettings{Model: "native", Effort: "high", FastMode: domain.AgentFastDefault}, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID, Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}, ApplyMode: domain.AgentSettingsRestart, CreatedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if started {
				execution, prepErr := f.m.prepareTUISettingsExecution(t.Context(), f.store, f.rec, op, false)
				if prepErr != nil {
					t.Fatal(prepErr)
				}
				op, err = f.store.PinAgentSettingsExecution(t.Context(), f.rec.ID, op.ID, op.Version, execution, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				op, err = f.store.AdvanceAgentSettingsOperation(t.Context(), f.rec.ID, op.ID, op.Version, domain.AgentSettingsApplyingModel, time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.m.recoverInterruptedInterfaceTransitions(t.Context()); err != nil {
				t.Fatal(err)
			}
			saved, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if f.runtime.created != 0 {
				t.Fatal("daemon recovery auto-launched a native writer")
			}
			release, admitted := f.m.AcquireSessionInput(f.rec.ID)
			if admitted {
				release()
			}
			if started {
				if saved.Phase != domain.AgentSettingsRecoveryRequired || admitted {
					t.Fatalf("crash lost recovery/input fence: %+v input=%v", saved, admitted)
				}
			} else {
				if saved.Phase != domain.AgentSettingsFailed || !admitted {
					t.Fatalf("unstarted crash stranded intact source: %+v input=%v", saved, admitted)
				}
			}
		})
	}
}

func (a *tuiSettingsAgent) PrepareTUIAgentSettings(ctx context.Context, cfg ports.RestoreConfig, source, target domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	a.lastSource = source
	if a.onPrepare != nil {
		if err := a.onPrepare(ctx); err != nil {
			return domain.AgentSettingsProviderPlan{}, err
		}
	}
	resolve := func(value domain.AgentSettings) domain.AgentSettingsNativeValues {
		model := string(value.Model)
		if model == "" {
			model = "native"
		}
		effort := string(value.Effort)
		if effort == "" {
			effort = "low"
			if a.baseline.Effort != "" {
				effort = a.baseline.Effort
			}
		}
		fast := string(value.FastMode)
		if value.FastMode == domain.AgentFastDefault && a.baseline.Fast != "" {
			fast = a.baseline.Fast
		}
		return domain.AgentSettingsNativeValues{Model: model, Effort: effort, Fast: fast}
	}
	return domain.AgentSettingsProviderPlan{Protocol: a.protocol, ProviderConversationID: cfg.Session.Metadata[ports.MetadataKeyAgentSessionID], Source: source, Target: target, Before: resolve(source), After: resolve(target)}, nil
}

func TestTUISettingsColdResumeReconstructsCurrentBaseline(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	f.agent.baseline = domain.AgentSettingsNativeValues{Effort: "low", Fast: "baseline-one"}
	zero := int64(0)
	reset, def := domain.AgentEffortSelection(""), domain.AgentFastDefault
	modelDefault := domain.AgentModelSelection("")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Model: &modelDefault, Effort: &reset, FastMode: &def}, IdempotencyKey: "default-resume"})
	if err != nil {
		t.Fatal(err)
	}
	if op = f.wait(t, op.ID); op.Phase != domain.AgentSettingsApplied {
		t.Fatalf("default reconfigure failed: %+v", op)
	}
	rec, _, err := f.store.GetSession(t.Context(), f.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.runtime.aliveByHandle[rec.Metadata.RuntimeHandleID] = false
	f.agent.baseline = domain.AgentSettingsNativeValues{Effort: "high", Fast: "baseline-two"}
	project, err := f.m.loadProject(t.Context(), rec.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.relaunchSession(t.Context(), "cold resume", rec, project, workspaceInfo(rec)); err != nil {
		t.Fatal(err)
	}
	last := f.agent.restores[len(f.agent.restores)-1]
	if last.Config.Effort != "high" || last.Config.Launch.NativeServiceTier != "baseline-two" || last.Session.Metadata[ports.MetadataKeyAgentSessionID] != "native-thread" {
		t.Fatalf("cold resume reused stale/native-implicit reset wire: %+v", last.Config)
	}
	if f.agent.lastSource != op.Target {
		t.Fatalf("cold reconstruction depended on historical source: %+v", f.agent.lastSource)
	}
	created := f.runtime.created
	rec, _, err = f.store.GetSession(t.Context(), f.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.runtime.aliveByHandle[rec.Metadata.RuntimeHandleID] = false
	f.agent.history = false
	if _, err := f.m.relaunchSession(t.Context(), "cold resume without history", rec, project, workspaceInfo(rec)); err == nil || f.runtime.created != created {
		t.Fatalf("cold reconstruction fell back to a fresh writer: %v creates=%d", err, f.runtime.created)
	}
	f.agent.history = true
	raw, _ := json.Marshal(ports.AgentModelCatalog{AgentID: "codex", Models: []ports.AgentModelInfo{{ID: "native", IsDefault: true, Efforts: []string{"low", "high"}, SupportsFast: true}}})
	if err := f.store.UpsertAgentModelCatalog(t.Context(), ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), InputFingerprint: "current", LastSuccessAt: time.Now().Add(-24 * time.Hour), FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.relaunchSession(t.Context(), "cold resume with pinned evidence", rec, project, workspaceInfo(rec)); err != nil || f.runtime.created != created+1 {
		t.Fatalf("cold reconstruction rejected pinned evidence: %v creates=%d", err, f.runtime.created)
	}
	last = f.agent.restores[len(f.agent.restores)-1]
	if last.Config.Effort != "low" || last.Config.Launch.NativeServiceTier != "baseline-one" || last.Session.Metadata[ports.MetadataKeyAgentSessionID] != "native-thread" {
		t.Fatalf("stale catalog did not retain applied native values: %+v", last)
	}
}

func TestTUISettingsExplicitRecoveryRestoresOnlyAfterSourceStops(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	f.runtime.destroyErr = errors.New("stop uncertain")
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "recover-source"})
	if err != nil {
		t.Fatal(err)
	}
	op = f.wait(t, op.ID)
	if op.Phase != domain.AgentSettingsRecoveryRequired {
		t.Fatalf("want recovery: %+v", op)
	}
	f.runtime.destroyErr = nil
	if _, err := f.m.ResumeAgentWithMode(t.Context(), f.rec.ID); err != nil {
		t.Fatalf("explicit recovery cannot restore a positively stopped source: %v", err)
	}
	current, err := f.store.SessionAgentSettings(t.Context(), f.rec.ID)
	if err != nil || current.Revision != 0 || current.Settings.Effort != "" || current.ProviderConversationID != "native-thread" {
		t.Fatalf("recovery changed durable preference or identity: %+v %v", current, err)
	}
	saved, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
	if err != nil || saved.Phase != domain.AgentSettingsFailed || saved.Checkpoint != domain.AgentSettingsRestored {
		t.Fatalf("recovery did not settle journal: %+v %v", saved, err)
	}
	if f.runtime.created != 1 {
		t.Fatalf("recovery started %d writers", f.runtime.created)
	}
	release, ok := f.m.AcquireSessionInput(f.rec.ID)
	if !ok {
		t.Fatal("verified restoration left input fenced")
	}
	release()
}

type tuiNoEffectError struct{ error }

func (tuiNoEffectError) PossibleHandle() ports.RuntimeHandle       { return ports.RuntimeHandle{} }
func (tuiNoEffectError) EffectOutcome() ports.RuntimeEffectOutcome { return ports.RuntimeEffectNone }
func (tuiNoEffectError) CleanupOutcome() ports.RuntimeCleanupOutcome {
	return ports.RuntimeCleanupNotAttempted
}

type tuiCommitFailureStore struct{ *sqlite.Store }

func (s tuiCommitFailureStore) CommitAgentSettingsOperation(context.Context, domain.SessionID, string, time.Time) (domain.AgentSettingsOperation, error) {
	return domain.AgentSettingsOperation{}, errors.New("native diagnostic must not be exposed")
}

type tuiExclusiveRuntime struct {
	*fakeRuntime
	overlap  bool
	unfenced bool
	store    *sqlite.Store
	id       domain.SessionID
}

func (r *tuiExclusiveRuntime) Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	if r.store != nil {
		op, found, err := r.store.PendingAgentSettingsOperation(ctx, r.id)
		if err != nil || !found || op.TUI == nil || !op.TUI.UnconfirmedRuntime {
			r.unfenced = true
		}
	}
	for _, alive := range r.aliveByHandle {
		if alive {
			r.overlap = true
		}
	}
	return r.fakeRuntime.Create(ctx, cfg)
}

func TestTUISettingsFailureRollsBackWithAtMostOneWriter(t *testing.T) {
	for _, fault := range []string{"launch", "commit"} {
		t.Run(fault, func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			rt := &tuiExclusiveRuntime{fakeRuntime: f.runtime, store: f.store, id: f.rec.ID}
			f.m.runtime = rt
			if fault == "launch" {
				f.runtime.createErrSequence = []error{tuiNoEffectError{errors.New("launch had no effects")}, nil}
			} else {
				f.m.store = tuiCommitFailureStore{f.store}
			}
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			on := domain.AgentFastOn
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high, FastMode: &on}, IdempotencyKey: fault})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsFailed || op.Checkpoint != domain.AgentSettingsRestored {
				t.Fatalf("failed target did not restore: %+v", op)
			}
			current, err := f.store.SessionAgentSettings(t.Context(), f.rec.ID)
			if err != nil || current.Revision != 0 || current.Settings.Effort != "" || current.Settings.FastMode != domain.AgentFastDefault || current.ProviderConversationID != "native-thread" {
				t.Fatalf("rollback changed source preference/identity: %+v %v", current, err)
			}
			if rt.overlap {
				t.Fatal("two native writers overlapped")
			}
			if rt.unfenced {
				t.Fatal("Create was not fenced durably before its side effect")
			}
			want := 1
			if fault == "commit" {
				want = 2
			}
			if rt.created != want {
				t.Fatalf("created=%d want=%d", rt.created, want)
			}
			last := f.agent.restores[len(f.agent.restores)-1]
			if last.Config.Effort != "low" || last.Session.Metadata[ports.MetadataKeyAgentSessionID] != "native-thread" {
				t.Fatalf("rollback did not use fresh source baseline: %+v", last)
			}
		})
	}
}

func TestTUISettingsRollbackLaunchFailureRetainsRecovery(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			rt := &tuiExclusiveRuntime{fakeRuntime: f.runtime, store: f.store, id: f.rec.ID}
			f.m.runtime = rt
			rollbackError := error(tuiNoEffectError{errors.New("rollback definitively did not launch")})
			if unknown {
				rollbackError = errors.New("rollback launch outcome unknown")
			}
			f.runtime.createErrSequence = []error{tuiNoEffectError{errors.New("target did not launch")}, rollbackError}
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "rollback-failed"})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsRecoveryRequired || op.Checkpoint != domain.AgentSettingsRollingBack || op.TUI.UnconfirmedRuntime != unknown || f.runtime.created != 0 {
				t.Fatalf("rollback failure lost its obligation: %+v creates=%d", op, f.runtime.created)
			}
			current, err := f.store.SessionAgentSettings(t.Context(), f.rec.ID)
			if err != nil || current.Revision != 0 || current.Settings != op.Source {
				t.Fatalf("rollback failure changed source preference: %+v %v", current, err)
			}
			if release, admitted := f.m.AcquireSessionInput(f.rec.ID); admitted {
				release()
				t.Fatal("failed rollback released input")
			}
			if _, err := f.m.recoverInterruptedInterfaceTransitions(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, err = f.m.ResumeAgentWithMode(t.Context(), f.rec.ID)
			if unknown {
				if err == nil || f.runtime.created != 0 {
					t.Fatalf("ambiguous rollback was retried: %v creates=%d", err, f.runtime.created)
				}
			} else if err != nil || f.runtime.created != 1 {
				t.Fatalf("proven no-effect rollback could not recover: %v creates=%d", err, f.runtime.created)
			}
			if rt.overlap || rt.unfenced {
				t.Fatalf("unsafe restoration: overlap=%v unfenced=%v", rt.overlap, rt.unfenced)
			}
		})
	}
}

func TestTUISettingsUnavailableHistoryFailsBeforeStopWithoutFreshFallback(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	f.agent.history = false
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	_, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "missing-history"})
	var structured *apierr.Error
	if !errors.As(err, &structured) || structured.Code != "AGENT_SETTINGS_NATIVE_HISTORY_UNAVAILABLE" {
		t.Fatalf("history failure lacks explicit reason: %v", err)
	}
	if f.runtime.created != 0 || f.runtime.destroyed != 0 || len(f.runtime.interrupts) != 0 {
		t.Fatalf("missing history touched source: %+v", f.runtime)
	}
}

func TestTUISettingsBusyDrainCanCancelWithoutInterrupt(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			if err := f.m.lcm.ApplyActivitySignal(t.Context(), f.rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Timestamp: time.Now(), LaunchID: f.rec.Metadata.RuntimeLaunchID}); err != nil {
				t.Fatal(err)
			}
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "busy-drain"})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				tr, found, err := f.store.GetSessionInterfaceTransition(t.Context(), op.ID)
				if err != nil || !found {
					t.Fatal(err)
				}
				if tr.Phase == domain.SessionInterfaceTransitionDraining {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("did not reach drain")
				}
				time.Sleep(time.Millisecond)
			}
			saved, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				err = f.m.CancelInterfaceTransition(t.Context(), f.rec.ID)
			} else {
				_, err = f.control.Cancel(t.Context(), agentsettings.Caller{}, f.rec.ID, op.ID, saved.Version)
			}
			if err != nil {
				t.Fatal(err)
			}
			saved = f.wait(t, op.ID)
			if saved.Phase != domain.AgentSettingsCancelled {
				t.Fatalf("busy cancellation=%+v", saved)
			}
			if f.runtime.created != 0 || f.runtime.destroyed != 0 || len(f.runtime.interrupts) != 0 {
				t.Fatal("drain without stopTurn stopped or interrupted active work")
			}

		})
	}
}

func TestTUISettingsPendingApprovalIsNotAnswered(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessClaudeCode)
	if err := f.m.lcm.ApplyActivitySignal(t.Context(), f.rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityBlocked, Timestamp: time.Now(), LaunchID: f.rec.Metadata.RuntimeLaunchID}); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "approval"})
	if err != nil {
		t.Fatal(err)
	}
	op = f.wait(t, op.ID)
	if op.Phase != domain.AgentSettingsFailed || op.ErrorCode != "AGENT_SETTINGS_APPROVAL_PENDING" {
		t.Fatalf("approval drain=%+v", op)
	}
	if f.runtime.created != 0 || f.runtime.destroyed != 0 || len(f.runtime.interrupts) != 0 {
		t.Fatal("approval was answered or interrupted")
	}
	release, ok := f.m.AcquireSessionInput(f.rec.ID)
	if !ok {
		t.Fatal("source approval cannot be answered after failed drain")
	}
	release()
}

func TestTUISettingsRecoveryDoesNotDispatchTransitionOutbox(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	f.runtime.destroyErr = context.DeadlineExceeded
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "outbox"})
	if err != nil {
		t.Fatal(err)
	}
	op = f.wait(t, op.ID)
	if err := f.store.EnqueueSessionInterfaceTransitionMessage(t.Context(), op.ID, "held", "keep exact text", time.Now(), ports.MessageDeliveryOptions{}); err != nil {
		t.Fatal(err)
	}
	tr, _, err := f.store.GetSessionInterfaceTransition(t.Context(), op.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := f.m.deliverTransitionMessages(ctx, tr); !errors.Is(err, ErrInterfaceTransitionInProgress) {
		t.Fatalf("recovery outbox was treated as deliverable: %v", err)
	}
	pending, err := f.store.ListPendingSessionInterfaceTransitionMessages(t.Context(), op.ID)
	if err != nil || len(pending) != 1 || pending[0].Message != "keep exact text" {
		t.Fatalf("recovery lost pending text: %+v %v", pending, err)
	}
}

func TestTUISettingsCrashWithTargetNeverCreatesSecondWriter(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprint(recorded), func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			rt := &tuiExclusiveRuntime{fakeRuntime: f.runtime}
			f.m.runtime = rt
			current, err := f.store.SessionAgentSettings(t.Context(), f.rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			op, err := f.store.BeginAgentSettingsOperation(t.Context(), domain.AgentSettingsOperation{ID: "target-crash", SessionID: f.rec.ID, IdempotencyKey: "target-crash", Source: current.Settings, Target: domain.AgentSettings{Model: "native", Effort: "high", FastMode: domain.AgentFastDefault}, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, BranchID: current.BranchID, Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}, ApplyMode: domain.AgentSettingsRestart, CreatedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			execution, err := f.m.prepareTUISettingsExecution(t.Context(), f.store, f.rec, op, false)
			if err != nil {
				t.Fatal(err)
			}
			op, err = f.store.PinAgentSettingsExecution(t.Context(), f.rec.ID, op.ID, op.Version, execution, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			op, err = f.store.AdvanceAgentSettingsOperation(t.Context(), f.rec.ID, op.ID, op.Version, domain.AgentSettingsApplyingModel, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.AdvanceSessionInterfaceTransition(t.Context(), op.ID, domain.SessionInterfaceTransitionRequested, domain.SessionInterfaceTransitionTargetStarting, op.ProviderConversationID, "", "", time.Now()); err != nil {
				t.Fatal(err)
			}
			f.runtime.aliveByHandle["source-runtime"] = false
			f.runtime.aliveByHandle["target-runtime"] = true
			if err := f.store.SetTUIAgentSettingsRuntimeUnknown(t.Context(), f.rec.ID, op.ID, true); err != nil {
				t.Fatal(err)
			}
			if recorded {
				if err := f.store.RecordTUIAgentSettingsRuntime(t.Context(), f.rec.ID, op.ID, op.TUI.TargetGeneration, "target-runtime", false); err != nil {
					t.Fatal(err)
				}
				rec, _, err := f.store.GetSession(t.Context(), f.rec.ID)
				if err != nil {
					t.Fatal(err)
				}
				rec.Metadata.RuntimeHandleID = "target-runtime"
				rec.Metadata.RuntimeLaunchID = op.TUI.TargetGeneration
				rec.Metadata.AgentSessionIDLaunchID = op.TUI.TargetGeneration
				if err := f.store.UpdateSession(t.Context(), rec); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.m.recoverInterruptedInterfaceTransitions(t.Context()); err != nil {
				t.Fatal(err)
			}
			if f.runtime.created != 0 {
				t.Fatal("crash recovery auto-launched a second writer")
			}
			if release, ok := f.m.AcquireSessionInput(f.rec.ID); ok {
				release()
				t.Fatal("crash recovery unfenced live/unknown target")
			}
			_, err = f.m.ResumeAgentWithMode(t.Context(), f.rec.ID)
			if recorded {
				if err != nil || rt.overlap || f.runtime.created != 1 {
					t.Fatalf("known target recovery did not stop then restore: %v overlap=%v created=%d", err, rt.overlap, f.runtime.created)
				}
			} else {
				if err == nil || f.runtime.created != 0 || f.runtime.destroyed != 0 {
					t.Fatalf("unknown target was guessed absent: %v created=%d destroyed=%d", err, f.runtime.created, f.runtime.destroyed)
				}
			}
		})
	}
}

func TestTUISettingsCrashDuringRollbackWithoutReceiptRemainsFenced(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	current, err := f.store.SessionAgentSettings(t.Context(), f.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.store.BeginAgentSettingsOperation(t.Context(), domain.AgentSettingsOperation{ID: "rollback-crash", SessionID: f.rec.ID, IdempotencyKey: "rollback-crash", Source: current.Settings, Target: domain.AgentSettings{Model: "native", Effort: "high", FastMode: domain.AgentFastDefault}, AccountID: current.AccountID, ControllerGeneration: current.OwnerGeneration, ProviderConversationID: current.ProviderConversationID, Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}, ApplyMode: domain.AgentSettingsRestart, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := f.m.prepareTUISettingsExecution(t.Context(), f.store, f.rec, op, false)
	if err != nil {
		t.Fatal(err)
	}
	op, err = f.store.PinAgentSettingsExecution(t.Context(), f.rec.ID, op.ID, op.Version, execution, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op, err = f.store.AdvanceAgentSettingsOperation(t.Context(), f.rec.ID, op.ID, op.Version, domain.AgentSettingsApplyingModel, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op, err = f.store.FinishAgentSettingsOperationWithCode(t.Context(), f.rec.ID, op.ID, op.Version, domain.AgentSettingsRecoveryRequired, domain.AgentSettingsErrorRecovery, "AGENT_SETTINGS_TARGET_FAILED", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	execution, err = f.m.prepareTUISettingsExecution(t.Context(), f.store, f.rec, op, true)
	if err != nil {
		t.Fatal(err)
	}
	recovery := domain.AgentSettingsRecovery{Owner: settingsOwner(current), Revision: current.Revision, Settings: current.Settings, Plan: &execution.Plan, Qualification: &execution.Qualification}
	if _, err := f.store.AdvanceAgentSettingsRecovery(t.Context(), f.rec.ID, op.ID, op.Version, domain.AgentSettingsRollingBack, recovery, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Create succeeded but the daemon crashed before its handle receipt and
	// session metadata were written. Absence of either is not death evidence.
	if err := f.store.SetTUIAgentSettingsRuntimeUnknown(t.Context(), f.rec.ID, op.ID, true); err != nil {
		t.Fatal(err)
	}
	f.runtime.aliveByHandle["source-runtime"] = false
	f.runtime.aliveByHandle["unrecorded-rollback"] = true
	if _, err := f.m.recoverInterruptedInterfaceTransitions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ResumeAgentWithMode(t.Context(), f.rec.ID); err == nil {
		t.Fatal("unrecorded rollback writer was guessed absent")
	}
	if f.runtime.created != 0 || f.runtime.destroyed != 0 {
		t.Fatalf("ambiguous rollback was retried: creates=%d destroys=%d", f.runtime.created, f.runtime.destroyed)
	}
	if release, admitted := f.m.AcquireSessionInput(f.rec.ID); admitted {
		release()
		t.Fatal("ambiguous rollback lost its input fence")
	}
}

func TestTUISettingsExcludesAccountInterfaceRestoreAndReaperRaces(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	log := []string{}
	accounts := newTransitionAccounts(&log)
	accounts.bound[f.rec.ID] = "device-codex"
	accounts.homes["device-codex"] = ""
	accounts.homes["managed"] = t.TempDir()
	f.m.SetAgentAccounts(accounts)
	entered := make(chan struct{}, 1)
	f.agent.onPrepare = func(ctx context.Context) error { entered <- struct{}{}; <-ctx.Done(); return ctx.Err() }
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "exclusive"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.m.transitionMu.Lock()
		if run := f.m.transitions[f.rec.ID]; run != nil {
			run.cancel()
		}
		f.m.transitionMu.Unlock()
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not begin")
	}
	if _, err := f.m.StartAccountTransition(t.Context(), f.rec.ID, "managed", domain.SessionInterfaceTransitionDrain); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("account move was not excluded: %v", err)
	}
	if _, err := f.m.StartInterfaceTransition(t.Context(), f.rec.ID, domain.SessionModeChat, domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("interface move was not excluded: %v", err)
	}
	if _, err := f.m.RestoreWithMode(t.Context(), f.rec.ID); err == nil {
		t.Fatal("restore raced reconfigure")
	}
	if _, err := f.m.ResumeAgentWithMode(t.Context(), f.rec.ID); err == nil {
		t.Fatal("resume raced reconfigure")
	}
	if !f.m.SessionMutationInProgress(f.rec.ID) {
		t.Fatal("reaper cannot see exclusive settings ownership")
	}
	if err := f.lifecycle.ApplyRuntimeObservation(t.Context(), f.rec.ID, ports.RuntimeFacts{ObservedAt: time.Now().Add(time.Hour), Runtime: ports.ProbeDead, Workload: ports.ProbeDead, LaunchID: f.rec.Metadata.RuntimeLaunchID}); err != nil {
		t.Fatal(err)
	}
	rec, _, err := f.store.GetSession(t.Context(), f.rec.ID)
	if err != nil || rec.IsTerminated {
		t.Fatalf("reaper terminated deliberate handoff: %+v %v", rec, err)
	}
	saved, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Cancel(t.Context(), agentsettings.Caller{}, f.rec.ID, op.ID, saved.Version); err != nil {
		t.Fatal(err)
	}
	saved = f.wait(t, op.ID)
	if saved.Phase != domain.AgentSettingsCancelled {
		t.Fatalf("cancel during read-only preparation failed: %+v", saved)
	}
	if f.runtime.created != 0 || f.runtime.destroyed != 0 || len(f.runtime.interrupts) != 0 {
		t.Fatal("competing operation touched source")
	}
}

func TestTUISettingsStopTurnIsExplicitAndPartOfRequestIdentity(t *testing.T) {
	for _, h := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		t.Run(string(h), func(t *testing.T) {
			f := newTUISettingsFixture(t, h)
			if err := f.lifecycle.ApplyActivitySignal(t.Context(), f.rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Timestamp: time.Now(), LaunchID: f.rec.Metadata.RuntimeLaunchID}); err != nil {
				t.Fatal(err)
			}
			signaled := make(chan error, 1)
			f.runtime.onInterrupt = func(ports.RuntimeHandle) {
				signaled <- f.lifecycle.ApplyActivitySignal(t.Context(), f.rec.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityIdle, Timestamp: time.Now(), LaunchID: f.rec.Metadata.RuntimeLaunchID})
			}
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			in := agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, StopTurn: true, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "explicit-interrupt"}
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsApplied || !op.StopTurn || len(f.runtime.interrupts) != 1 {
				t.Fatalf("explicit interruption was lost or repeated: %+v interrupts=%v", op, f.runtime.interrupts)
			}
			if err := <-signaled; err != nil {
				t.Fatal(err)
			}
			retry, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, in)
			if err != nil || retry.ID != op.ID || f.runtime.created != 1 {
				t.Fatalf("idempotent restart launched again: %+v %v created=%d", retry, err, f.runtime.created)
			}
			in.StopTurn = false
			if _, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, in); err == nil {
				t.Fatal("same key silently changed interruption consent")
			}
		})
	}
}

func TestTUISettingsColdResumeFallsBackOnlyToPinnedNativeValues(t *testing.T) {
	for _, failure := range []string{"stale", "refresh-error", "probe", "fingerprint", "model-removed", "effort-removed"} {
		t.Run(failure, func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			f.agent.baseline = domain.AgentSettingsNativeValues{Effort: "low", Fast: "pinned-tier"}
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "fallback"})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsApplied {
				t.Fatalf("not applied: %+v", op)
			}
			rec, _, err := f.store.GetSession(t.Context(), f.rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.runtime.aliveByHandle[rec.Metadata.RuntimeHandleID] = false
			if failure == "probe" {
				f.agent.onPrepare = func(context.Context) error { return errors.New("probe failed") }
			} else {
				catalog, _, err := f.store.GetAgentModelCatalog(t.Context(), "codex", "")
				if err != nil {
					t.Fatal(err)
				}
				switch failure {
				case "stale":
					catalog.LastSuccessAt = time.Now().Add(-24 * time.Hour)
				case "refresh-error":
					catalog.RefreshError = "discovery unavailable"
				case "fingerprint":
					catalog.InputFingerprint = "previous-binary"
				case "model-removed":
					catalog.CatalogJSON = `{"agentId":"codex","models":[{"id":"replacement","isDefault":true,"efforts":["low","high"],"supportsFast":true}]}`
				case "effort-removed":
					catalog.CatalogJSON = `{"agentId":"codex","models":[{"id":"native","isDefault":true,"efforts":["low"],"supportsFast":true}]}`
				}
				if err := f.store.UpsertAgentModelCatalog(t.Context(), catalog); err != nil {
					t.Fatal(err)
				}
			}
			project, err := f.m.loadProject(t.Context(), rec.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.m.relaunchSession(t.Context(), "cold resume", rec, project, workspaceInfo(rec))
			if err != nil || result.Mode != RestoreModeNative {
				t.Fatalf("pinned native resume: %+v %v", result, err)
			}
			last := f.agent.restores[len(f.agent.restores)-1]
			if last.Config.Effort != "high" || last.Config.Launch.NativeServiceTier != "pinned-tier" || last.Session.Metadata[ports.MetadataKeyAgentSessionID] != "native-thread" {
				t.Fatalf("wrong pinned wire: %+v", last)
			}
			f.agent.history = false
			created := f.runtime.created
			rec, _, _ = f.store.GetSession(t.Context(), f.rec.ID)
			if _, err = f.m.relaunchSession(t.Context(), "no native history", rec, project, workspaceInfo(rec)); err == nil || f.runtime.created != created {
				t.Fatalf("fallback created fresh writer: %v", err)
			}
		})
	}
}

func TestTUISettingsRecoveryUsesPinnedSourceWhenDiscoveryFails(t *testing.T) {
	for _, failure := range []string{"stale", "refresh-error", "probe"} {
		t.Run(failure, func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			f.agent.baseline = domain.AgentSettingsNativeValues{Effort: "low", Fast: "source-tier"}
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "committed"})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsApplied {
				t.Fatal(op.Phase)
			}
			one := int64(1)
			low := domain.AgentEffortSelection("low")
			f.runtime.destroyErr = errors.New("stop uncertain")
			op, err = f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &one, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &low}, IdempotencyKey: "recover"})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsRecoveryRequired {
				t.Fatal(op.Phase)
			}
			f.runtime.destroyErr = nil
			if failure == "probe" {
				f.agent.onPrepare = func(context.Context) error { return errors.New("probe failed") }
			} else {
				catalog, _, err := f.store.GetAgentModelCatalog(t.Context(), "codex", "")
				if err != nil {
					t.Fatal(err)
				}
				if failure == "stale" {
					catalog.LastSuccessAt = time.Now().Add(-24 * time.Hour)
				} else {
					catalog.RefreshError = "discovery failed"
				}
				if err = f.store.UpsertAgentModelCatalog(t.Context(), catalog); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = f.m.ResumeAgentWithMode(t.Context(), f.rec.ID); err != nil {
				t.Fatal(err)
			}
			saved, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
			if err != nil || saved.Phase != domain.AgentSettingsFailed || saved.Checkpoint != domain.AgentSettingsRestored {
				t.Fatalf("recovery stranded: %+v %v", saved, err)
			}
			last := f.agent.restores[len(f.agent.restores)-1]
			if last.Config.Effort != "high" || last.Config.Launch.NativeServiceTier != "source-tier" {
				t.Fatalf("wrong restored pin: %+v", last)
			}
		})
	}
}

func TestTUISettingsKillSettlesRecoveryWithoutInventingWriterAbsence(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			if unknown {
				f.runtime.createErr = errors.New("unknown launch outcome")
			} else {
				f.runtime.destroyErr = errors.New("source stop uncertain")
			}
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "kill-recovery"})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsRecoveryRequired || op.TUI.UnconfirmedRuntime != unknown {
				t.Fatalf("missing recovery: %+v", op)
			}
			if _, err := f.store.AbandonTerminatedTUISettings(t.Context(), f.rec.ID, op.ID, op.Version, !unknown, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
				t.Fatalf("live operation abandoned: %v", err)
			}
			f.runtime.destroyErr = nil
			f.runtime.createErr = nil
			created := f.runtime.created
			freed, err := f.m.Kill(t.Context(), f.rec.ID)
			if err != nil {
				t.Fatalf("kill recovery: %v", err)
			}
			rec, _, err := f.store.GetSession(t.Context(), f.rec.ID)
			if err != nil || !rec.IsTerminated {
				t.Fatalf("not terminated: %+v %v", rec, err)
			}
			saved, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
			if err != nil || saved.Phase != domain.AgentSettingsCancelled || saved.TUI.UnconfirmedRuntime != unknown {
				t.Fatalf("termination did not settle journal: %+v %v", saved, err)
			}
			if _, pending, err := f.store.PendingAgentSettingsOperation(t.Context(), f.rec.ID); err != nil || pending {
				t.Fatalf("termination retained pending: %v %v", pending, err)
			}
			if active, err := f.m.hasActiveInterfaceTransition(t.Context(), f.rec.ID); err != nil || active {
				t.Fatalf("termination retained transition: %v %v", active, err)
			}
			if _, err := f.m.recoverInterruptedInterfaceTransitions(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.m.agentOpMu.Lock()
			fence := f.m.tuiSettingsFences[f.rec.ID]
			f.m.agentOpMu.Unlock()
			if fence != "" {
				t.Fatal("terminated operation re-fenced after startup")
			}
			if unknown {
				if freed {
					t.Fatal("unknown writer workspace removed")
				}
				project, err := f.m.loadProject(t.Context(), rec.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				for _, fresh := range []bool{false, true} {
					_, err = f.m.relaunchSessionWithPolicyAndGeneration(t.Context(), "abandoned writer", rec, project, workspaceInfo(rec), nil, fresh, false, "", domain.SessionInterfaceTransitionHistoryStrict, nil)
					if err == nil {
						t.Fatal("abandonment allowed another writer")
					}
				}
				freed, err = f.m.Kill(t.Context(), f.rec.ID)
				if err != nil || freed {
					t.Fatalf("repeat kill destroyed unconfirmed workspace: %v %v", freed, err)
				}
			}
			if f.runtime.created != created {
				t.Fatal("kill/recovery created a native writer")
			}
		})
	}
}

func TestTUISettingsTerminatedCommittedSessionStillResumesNative(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "terminated-committed"})
	if err != nil {
		t.Fatal(err)
	}
	op = f.wait(t, op.ID)
	if op.Phase != domain.AgentSettingsApplied {
		t.Fatal(op.Phase)
	}
	rec, _, err := f.store.GetSession(t.Context(), f.rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.runtime.aliveByHandle[rec.Metadata.RuntimeHandleID] = false
	if err = f.lifecycle.MarkTerminated(t.Context(), rec.ID); err != nil {
		t.Fatal(err)
	}
	rec, _, err = f.store.GetSession(t.Context(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	project, err := f.m.loadProject(t.Context(), rec.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.m.relaunchSession(t.Context(), "restore", rec, project, workspaceInfo(rec))
	if err != nil || result.Mode != RestoreModeNative {
		t.Fatalf("terminated committed restore: %+v %v", result, err)
	}
}

func TestTUISettingsTerminatedRecoveryStartupSettlesWithoutLaunching(t *testing.T) {
	f := newTUISettingsFixture(t, domain.HarnessCodex)
	f.runtime.createErr = errors.New("unknown launch")
	zero := int64(0)
	high := domain.AgentEffortSelection("high")
	op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "terminated-recovery"})
	if err != nil {
		t.Fatal(err)
	}
	op = f.wait(t, op.ID)
	if op.Phase != domain.AgentSettingsRecoveryRequired {
		t.Fatal(op.Phase)
	}
	if err = f.lifecycle.MarkTerminated(t.Context(), f.rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.AbandonTerminatedTUISettings(t.Context(), f.rec.ID, op.ID, op.Version, true, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("unknown writer accepted as shut down: %v", err)
	}
	if _, err = f.store.AbandonTerminatedTUISettings(t.Context(), f.rec.ID, op.ID, op.Version+1, false, time.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("stale termination CAS accepted: %v", err)
	}
	for range 2 {
		if _, err = f.m.recoverInterruptedInterfaceTransitions(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := f.store.AgentSettingsOperation(t.Context(), f.rec.ID, op.ID)
	if err != nil || saved.Phase != domain.AgentSettingsCancelled || saved.TUI.TerminationVerified || !saved.TUI.UnconfirmedRuntime {
		t.Fatalf("terminated recovery unsettled/lost evidence: %+v %v", saved, err)
	}
	active, err := f.m.hasActiveInterfaceTransition(t.Context(), f.rec.ID)
	if err != nil || active || f.runtime.created != 0 {
		t.Fatalf("startup kept active/created writer: %v %v %d", active, err, f.runtime.created)
	}
	blocked, err := f.store.HasAbandonedTUISettingsWriter(t.Context(), f.rec.ID)
	if err != nil || !blocked {
		t.Fatalf("startup abandoned launch veto: %v %v", blocked, err)
	}
}

func TestTUISettingsPinnedFallbackRejectsChangedAccountAndCancelledProbe(t *testing.T) {
	for _, failure := range []string{"account", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessCodex)
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high}, IdempotencyKey: "guarded-pin"})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsApplied {
				t.Fatal(op.Phase)
			}
			rec, _, err := f.store.GetSession(t.Context(), f.rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.runtime.aliveByHandle[rec.Metadata.RuntimeHandleID] = false
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.agent.onPrepare = func(context.Context) error {
				if failure == "cancelled" {
					cancel()
					return context.Canceled
				}
				if err := f.store.BindSessionAgentAccount(t.Context(), f.rec.ID, domain.AgentAccountProviderCodex, "replacement-account", time.Now()); err != nil {
					return err
				}
				return errors.New("probe failed after owner changed")
			}
			project, err := f.m.loadProject(t.Context(), rec.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			created := f.runtime.created
			if _, err = f.m.relaunchSession(ctx, "guarded cold resume", rec, project, workspaceInfo(rec)); err == nil || f.runtime.created != created {
				t.Fatalf("unsafe fallback: %v creates=%d", err, f.runtime.created)
			}
		})
	}
}

func TestClaudeTUISettingsColdResumeUsesPinnedFastChoiceWhenProbeFails(t *testing.T) {
	for _, fast := range []domain.AgentFastMode{domain.AgentFastOn, domain.AgentFastOff} {
		t.Run(string(fast), func(t *testing.T) {
			f := newTUISettingsFixture(t, domain.HarnessClaudeCode)
			zero := int64(0)
			high := domain.AgentEffortSelection("high")
			op, err := f.control.Change(t.Context(), agentsettings.Caller{}, f.rec.ID, agentsettings.Change{ExpectedRevision: &zero, ApplyMode: domain.AgentSettingsRestart, Settings: domain.AgentSettingsPatch{Effort: &high, FastMode: &fast}, IdempotencyKey: "claude-cold-pin"})
			if err != nil {
				t.Fatal(err)
			}
			op = f.wait(t, op.ID)
			if op.Phase != domain.AgentSettingsApplied {
				t.Fatal(op.Phase)
			}
			rec, _, err := f.store.GetSession(t.Context(), f.rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.runtime.aliveByHandle[rec.Metadata.RuntimeHandleID] = false
			f.agent.onPrepare = func(context.Context) error { return ports.ErrAgentSettingsCapabilitiesUnavailable }
			project, err := f.m.loadProject(t.Context(), rec.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.m.relaunchSession(t.Context(), "Claude cold resume", rec, project, workspaceInfo(rec))
			if err != nil || result.Mode != RestoreModeNative {
				t.Fatalf("Claude pinned resume: %v", err)
			}
			last := f.agent.restores[len(f.agent.restores)-1]
			if last.Config.Effort != "high" || last.Config.Launch.FastMode != (fast == domain.AgentFastOn) || last.Config.Launch.FastModeOff != (fast == domain.AgentFastOff) || last.Session.Metadata[ports.MetadataKeyAgentSessionID] != "native-thread" {
				t.Fatalf("lost Claude native wire: %+v", last)
			}
		})
	}
}
