package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type grantFailureRuntime struct{ fakeRuntime }

func (*grantFailureRuntime) Create(_ context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	return ports.RuntimeHandle{}, errors.New("failed native launch " + cfg.Env[ports.EnvAgentSettingsGrant])
}

func TestRuntimeSettingsGrantIsRedactedFromLaunchFailure(t *testing.T) {
	store := newFakeStore()
	store.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: testRoleAgents()}
	m := New(Deps{Runtime: &grantFailureRuntime{}, Agents: singleAgent{agent: generationEnvAgent{}}, Workspace: &fakeWorkspace{}, Store: store, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: store}, DataDir: t.TempDir(), LookPath: func(string) (string, error) { return "/bin/true", nil }, NewLaunchID: func() string { return "final-launch" }, Executable: func() (string, error) { return "/opt/ao", nil }})
	m.SetAgentSettingsGrantEnvironment(&settingsEnvIssuer{})
	_, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker})
	if err == nil {
		t.Fatal("expected fake launch failure")
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), "scoped-fake") {
			t.Fatal("native grant escaped through launch error chain")
		}
	}
}

type settingsEnvIssuer struct {
	generation string
	harness    domain.AgentHarness
	mode       domain.SessionMode
}

func (i *settingsEnvIssuer) AgentSettingsGrantEnv(_ context.Context, _ domain.SessionID, h domain.AgentHarness, m domain.SessionMode, g string) (map[string]string, error) {
	i.generation = g
	i.harness = h
	i.mode = m
	return map[string]string{ports.EnvAgentSettingsGrant: "scoped-fake"}, nil
}

func TestRuntimeSettingsGrantUsesFinalLaunchGeneration(t *testing.T) {
	store := newFakeStore()
	store.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: testRoleAgents()}
	runtime := &fakeRuntime{}
	m := New(Deps{Runtime: runtime, Agents: singleAgent{agent: generationEnvAgent{}}, Workspace: &fakeWorkspace{}, Store: store, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: store}, DataDir: t.TempDir(), LookPath: func(string) (string, error) { return "/bin/true", nil }, NewLaunchID: func() string { return "final-launch" }, Executable: func() (string, error) { return "/opt/ao", nil }})
	issuer := &settingsEnvIssuer{}
	m.SetAgentSettingsGrantEnvironment(issuer)
	if _, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker}); err != nil {
		t.Fatal(err)
	}
	if issuer.generation != "final-launch" || issuer.mode != domain.SessionModeTUI || issuer.harness == "" || runtime.lastCfg.Env[ports.EnvAgentSettingsGrant] != "scoped-fake" {
		t.Fatalf("grant did not use final generation: %+v", issuer)
	}
}

type generationEnvAgent struct{ fakeAgent }

func (generationEnvAgent) AugmentRuntimeLaunchEnv(env map[string]string, dataDir string, id domain.SessionID, launchID string) {
	env["augmentation"] = dataDir + ":" + string(id) + ":" + launchID + ":" + env[EnvRuntimeLaunchID]
}

func TestRuntimeLaunchEnvironmentAcrossSpawnAndRestore(t *testing.T) {
	for _, operation := range []string{"spawn", "restore"} {
		t.Run(operation, func(t *testing.T) {
			store := newFakeStore()
			store.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: testRoleAgents()}
			runtime := &fakeRuntime{}
			dataDir := t.TempDir()
			m := New(Deps{Runtime: runtime, Agents: singleAgent{agent: generationEnvAgent{}}, Workspace: &fakeWorkspace{}, Store: store, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: store}, DataDir: dataDir,
				LookPath: func(string) (string, error) { return "/bin/true", nil }, NewLaunchID: func() string { return "launch-new" }, Executable: func() (string, error) { return "/opt/ao", nil }})
			var id domain.SessionID
			if operation == "spawn" {
				rec, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker})
				if err != nil {
					t.Fatal(err)
				}
				id = rec.ID
			} else {
				seedTerminal(store, "mer-1", domain.SessionMetadata{WorkspacePath: "/ws/mer-1", Branch: "branch", AgentSessionID: "native-1", RuntimeLaunchID: "launch-old"})
				result, err := m.RestoreWithMode(ctx, "mer-1")
				if err != nil {
					t.Fatal(err)
				}
				id = result.Session.ID
			}
			if got, want := runtime.lastCfg.Env["augmentation"], dataDir+":"+string(id)+":launch-new:launch-new"; got != want {
				t.Fatalf("%s augmentation=%q, want %q", operation, got, want)
			}
		})
	}
}

func TestRuntimeLaunchEnvironmentAugmentedAfterGeneration(t *testing.T) {
	for _, force := range []bool{false, true} {
		m := New(Deps{DataDir: "/ao-data", Executable: func() (string, error) { return "/ao", nil }})
		env := map[string]string{}
		if _, err := m.wrapAgentProcessWithLaunchID(generationEnvAgent{}, "mer-1", env, []string{"fx"}, "launch-new", force); err != nil {
			t.Fatal(err)
		}
		if env["augmentation"] != "/ao-data:mer-1:launch-new:launch-new" {
			t.Fatalf("force=%v: augmentation = %q; optional capability must run with the assigned generation", force, env["augmentation"])
		}
	}
}
