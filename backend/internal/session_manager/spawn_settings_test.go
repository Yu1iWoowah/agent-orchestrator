package sessionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSpawnFastConfigPrecedence(t *testing.T) {
	var project domain.ProjectConfig
	if err := json.Unmarshal([]byte(`{"agentConfig":{"fastMode":"on"},"worker":{"agent":"codex","agentConfig":{"fastMode":"off"}}}`), &project); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		harness    domain.AgentHarness
		task, want string
	}{
		{domain.HarnessCodex, `{}`, "off"},
		{domain.HarnessClaudeCode, `{}`, "on"},
		{domain.HarnessCodex, `{"fastMode":"default"}`, "default"},
	} {
		var task domain.AgentConfig
		if err := json.Unmarshal([]byte(tc.task), &task); err != nil {
			t.Fatal(err)
		}
		got := applySpawnAgentConfig(effectiveAgentConfig(tc.harness, domain.KindWorker, project), task)
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["fastMode"] != tc.want {
			t.Fatalf("harness=%s task=%s got=%s want fast=%s", tc.harness, tc.task, raw, tc.want)
		}
	}
}

type spawnSettingsPreflight struct {
	accounts *transitionAccounts
	t        *testing.T
	err      error
	called   bool
	calls    int
}

func (s *fakeStore) SaveSpawnAgentSettings(_ context.Context, id domain.SessionID, resolution domain.SpawnAgentSettingsResolution) error {
	rec := s.sessions[id]
	rec.Metadata.Model = string(resolution.Settings.Model)
	rec.Metadata.Effort = string(resolution.Settings.Effort)
	rec.Metadata.FastMode = resolution.Settings.FastMode
	rec.Metadata.SpawnAgentSettings = &resolution
	s.sessions[id] = rec
	return nil
}

func (f *spawnSettingsPreflight) ValidateSpawn(ctx context.Context, id domain.SessionID, settings domain.AgentSettings, _ bool) (domain.AgentSettings, error) {
	f.called = true
	f.calls++
	account, err := f.accounts.SessionAccountID(ctx, id, domain.HarnessCodex)
	if err != nil || account != "selected-account" {
		f.t.Fatalf("preflight before binding: account=%q err=%v", account, err)
	}
	if settings.Model != "" || settings.Effort != "" || settings.FastMode != domain.AgentFastOff {
		f.t.Fatalf("reset lost: %+v", settings)
	}
	return settings, f.err
}

func TestSpawnTypedSettingsResetAndRejectBeforeWorkspace(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "reset", true: "reject"}[reject], func(t *testing.T) {
			st := newFakeStore()
			st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: domain.ProjectConfig{AgentConfig: domain.AgentConfig{Model: "base", Effort: "high"}, Worker: domain.RoleOverride{Harness: domain.HarnessCodex, AgentConfig: domain.AgentConfig{Model: "role"}}}}
			agent := &recordingAgent{}
			rt := &fakeRuntime{}
			ws := &fakeWorkspace{}
			m := New(Deps{Runtime: rt, Agents: singleAgent{agent: agent}, Workspace: ws, Store: st, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: st}, LookPath: func(string) (string, error) { return "/bin/true", nil }})
			log := []string{}
			accounts := newTransitionAccounts(&log)
			m.SetAgentAccounts(accounts)
			preflight := &spawnSettingsPreflight{accounts: accounts, t: t}
			if reject {
				preflight.err = errors.New("unknown fast evidence")
			}
			setter, ok := any(m).(interface {
				SetAgentSettingsPreflight(ports.AgentSettingsPreflight)
			})
			if !ok {
				t.Fatal("spawn settings preflight is not wired")
			}
			setter.SetAgentSettingsPreflight(preflight)
			cfg := ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, AgentAccountID: "selected-account"}
			if !reject {
				cfg.ClientRequestID = "settings-retry"
				cfg.ClientRequestHash = "same-payload"
			}
			if err := json.Unmarshal([]byte(`{"agentSettings":{"model":null,"effort":null,"fastMode":"off"}}`), &cfg); err != nil {
				t.Fatal(err)
			}
			rec, _, _, err := m.Spawn(context.Background(), cfg)
			if reject {
				if err == nil || ws.createCount != 0 || rt.created != 0 || len(st.sessions) != 0 {
					t.Fatalf("rejected spawn changed state: err=%v workspace=%d sessions=%d", err, ws.createCount, len(st.sessions))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !preflight.called || agent.lastConfig.Model != "" || agent.lastConfig.Effort != "" || !agent.lastConfig.Launch.FastModeOff {
				t.Fatalf("launch=%+v preflight=%v", agent.lastConfig, preflight.called)
			}
			if rec.Metadata.Model != "" || rec.Metadata.Effort != "" {
				t.Fatalf("unpinned reset: %+v", rec.Metadata)
			}
			if rec.Metadata.SpawnAgentSettings == nil || rec.Metadata.SpawnAgentSettings.Sources.Model != "request" || rec.Metadata.SpawnAgentSettings.Sources.FastMode != "request" {
				t.Fatalf("missing spawn sources: %+v", rec.Metadata.SpawnAgentSettings)
			}
			project := st.projects["mer"]
			project.Config.AgentConfig.Model = "changed-default"
			project.Config.AgentConfig.FastMode = domain.AgentFastOn
			st.projects["mer"] = project
			replayed, _, _, err := m.Spawn(context.Background(), cfg)
			if err != nil || preflight.calls != 1 || ws.createCount != 1 || !reflect.DeepEqual(replayed.Metadata.SpawnAgentSettings, rec.Metadata.SpawnAgentSettings) {
				t.Fatalf("retry reselected settings: replay=%+v calls=%d workspace=%d err=%v", replayed.Metadata.SpawnAgentSettings, preflight.calls, ws.createCount, err)
			}
		})
	}
}

func TestRoleModelChangeMarksInheritedEffortForValidation(t *testing.T) {
	cfg := ports.SpawnConfig{Harness: domain.HarnessCodex, Kind: domain.KindWorker}
	project := domain.ProjectConfig{AgentConfig: domain.AgentConfig{Model: "project-model", Effort: "high"}, Worker: domain.RoleOverride{Harness: domain.HarnessCodex, AgentConfig: domain.AgentConfig{Model: "role-model"}}}
	_, resolution, clearEffort, err := resolveSpawnSettings(cfg, project)
	if err != nil || !clearEffort || resolution.Sources.Model != "role" || resolution.Sources.Effort != "project" {
		t.Fatalf("resolution=%+v clear=%v err=%v", resolution, clearEffort, err)
	}
}

func TestRestoredSettingsDoNotInheritNewProjectPreferences(t *testing.T) {
	rec := domain.SessionRecord{Harness: domain.HarnessCodex, Kind: domain.KindWorker, Metadata: domain.SessionMetadata{FastMode: domain.AgentFastOff}}
	got := restoredAgentConfig(rec, domain.ProjectConfig{AgentConfig: domain.AgentConfig{Model: "changed", Effort: "high", FastMode: domain.AgentFastOn}})
	if got.Model != "" || got.Effort != "" || got.FastMode != domain.AgentFastOff {
		t.Fatalf("restored changed defaults: %+v", got)
	}
}

func TestTypedSpawnStaticModelValidationUsesSettingsError(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer"}
	ws := &fakeWorkspace{}
	m := New(Deps{Store: st, Workspace: ws, Agents: singleAgent{agent: &recordingAgent{}}, Runtime: &fakeRuntime{}, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: st}})
	model := domain.AgentModelSelection("not-a-native-memcode-model")
	_, _, _, err := m.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessMemcode, AgentSettings: &domain.AgentSettingsPatch{Model: &model}})
	if !errors.Is(err, ports.ErrAgentSettingsInvalid) || ws.createCount != 0 || len(st.sessions) != 0 {
		t.Fatalf("err=%v workspace=%d sessions=%d", err, ws.createCount, len(st.sessions))
	}
}

func TestSpawnModelResetClearsInheritedHarnessMode(t *testing.T) {
	reset := domain.AgentModelSelection("")
	cfg := ports.SpawnConfig{Harness: domain.HarnessAmp, Kind: domain.KindWorker, AgentSettings: &domain.AgentSettingsPatch{Model: &reset}}
	resolved, _, _, err := resolveSpawnSettings(cfg, domain.ProjectConfig{AgentConfig: domain.AgentConfig{Mode: "high"}})
	launch := normalizeAgentConfigForHarness(cfg.Harness, resolved)
	if err != nil || launch.Model != "" || launch.Mode != "" {
		t.Fatalf("provider reset retained mode override: config=%+v err=%v", launch, err)
	}
}

func TestRestoredModeDefaultDoesNotInheritProjectMode(t *testing.T) {
	rec := domain.SessionRecord{Harness: domain.HarnessAmp, Kind: domain.KindWorker}
	project := domain.ProjectConfig{AgentConfig: domain.AgentConfig{Mode: "high"}}
	launch := normalizeAgentConfigForHarness(rec.Harness, restoredAgentConfig(rec, project))
	if launch.Model != "" || launch.Mode != "" {
		t.Fatalf("restored provider default inherited project mode: %+v", launch)
	}
}
