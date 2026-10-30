package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// Real profile boundaries and durable bindings are composed with fake native
// launch/identity. No real device files or provider binaries are consulted.
func TestContainedAccountHandoffProtectsProfilesAndPreservesNativeHistory(t *testing.T) {
	for _, p := range domain.AgentAccountProviders {
		for _, mode := range []domain.SessionMode{domain.SessionModeTUI, domain.SessionModeChat} {
			t.Run(string(p)+"/"+string(mode), func(t *testing.T) {
				ctx := context.Background()
				root := filepath.Join(t.TempDir(), "accounts")
				device := t.TempDir()
				deviceFile := filepath.Join(device, "settings.json")
				if err := os.WriteFile(deviceFile, []byte("fake-device-credential-canary"), 0644); err != nil {
					t.Fatal(err)
				}
				before, err := os.Stat(deviceFile)
				if err != nil {
					t.Fatal(err)
				}
				harness, envKey, historyDir, credential := domain.HarnessClaudeCode, "CLAUDE_CONFIG_DIR", "projects", ".credentials.json"
				var adapter ports.AgentAccountProviderCLI = &agentaccount.Claude{DeviceHome: device}
				if p == domain.AgentAccountProviderCodex {
					harness, envKey, historyDir, credential = domain.HarnessCodex, "CODEX_HOME", "sessions", "auth.json"
					adapter = &agentaccount.Codex{DeviceHome: device}
				}
				provider := &containedHandoffProvider{AgentAccountProviderCLI: adapter}
				db := sqlitetest.MustOpen(t)
				now := time.Now().UTC()
				if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "session", Path: root, RegisteredAt: now}); err != nil {
					t.Fatal(err)
				}
				worker, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "session", Harness: harness, Kind: domain.KindWorker, Mode: mode, CreatedAt: now})
				if err != nil || worker.ID != "session-1" {
					t.Fatal("invalid parent fixture", worker.ID, err)
				}
				svc := agentaccounts.New(db, []ports.AgentAccountProviderCLI{provider}, root, nil, nil)
				for _, id := range []string{"A", "B"} {
					home := filepath.Join(root, string(p), id)
					if err := os.MkdirAll(home, 0700); err != nil {
						t.Fatal(err)
					}
					if err := provider.Prepare(home); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(home, credential), []byte("fake-private-"+id), 0600); err != nil {
						t.Fatal(err)
					}
					if err := db.CreateAgentAccount(ctx, domain.AgentAccount{ID: id, Provider: p, Home: home, ProfileKind: domain.AgentAccountProfileContained, Email: id + "@example.invalid", CreatedAt: time.Now().UTC()}); err != nil {
						t.Fatal(err)
					}
				}
				history := filepath.Join(root, string(p), "A", historyDir, "native-1")
				if err := os.WriteFile(history, []byte("same-native-conversation"), 0600); err != nil {
					t.Fatal(err)
				}
				m, st, runtime, chat, _, _ := newAccountTransitionManager(t, mode)
				source := st.sessions["session-1"]
				source.Harness = harness
				st.sessions[source.ID] = source
				m.SetAgentAccounts(svc)
				if err := svc.BindSession(ctx, "session-1", harness, "A"); err != nil {
					t.Fatal(err)
				}
				checked := false
				check := func() {
					if checked {
						return
					}
					checked = true
					for _, id := range []string{"A", "B"} {
						if err := svc.Remove(ctx, id); !errors.Is(err, ports.ErrAgentAccountInUse) {
							t.Errorf("in-flight %s profile remove: %v", id, err)
						}
						if _, err := svc.StartSignInAgain(ctx, id); !errors.Is(err, ports.ErrAgentAccountInUse) {
							t.Errorf("in-flight %s profile login-again: %v", id, err)
						}
					}
				}
				nativeAgent := containedHistoryAgent{envKey: envKey, historyDir: historyDir}
				if mode == domain.SessionModeTUI {
					runtime.onCreate = check
				} else {
					// Provider-default settings legitimately skip discovery.
					// Check the lease at the native read, not an optional catalog call.
					nativeAgent.beforeNativeProbe = func(home string) {
						if home == filepath.Join(root, string(p), "B") {
							check()
						}
					}
				}
				m.agents = singleAgent{agent: nativeAgent}
				transition, err := m.StartAccountTransition(ctx, "session-1", "B", domain.SessionInterfaceTransitionDrain)
				if err != nil {
					t.Fatal(err)
				}
				settled := awaitTransition(t, st, transition.ID)
				if settled.Phase != domain.SessionInterfaceTransitionCompleted {
					t.Fatalf("contained handoff failed: %+v", settled)
				}
				launchedEnv := runtime.lastCfg.Env
				if mode == domain.SessionModeChat {
					launchedEnv = chat.start.Env
					if chat.start.ProviderConversationID != "native-1" {
						t.Fatal("Chat lost native conversation")
					}
				}
				if launchedEnv[envKey] != filepath.Join(root, string(p), "B") {
					t.Fatal("native controller used another account", launchedEnv)
				}
				if !checked {
					t.Fatal("native boundary guard not exercised")
				}
				account, err := svc.SessionAccountID(ctx, "session-1", harness)
				if err != nil || account != "B" {
					t.Fatalf("durable binding=%s %v", account, err)
				}
				current, _, err := st.GetSession(ctx, "session-1")
				if err != nil || current.Metadata.AgentSessionID != "native-1" {
					t.Fatal("native conversation changed", current.Metadata, err)
				}
				for _, id := range []string{"A", "B"} {
					data, err := os.ReadFile(filepath.Join(root, string(p), id, credential))
					if err != nil || string(data) != "fake-private-"+id {
						t.Fatal("private credential boundary changed", id, err)
					}
				}
				content, err := os.ReadFile(deviceFile)
				after, statErr := os.Stat(deviceFile)
				if err != nil || statErr != nil || string(content) != "fake-device-credential-canary" || after.Mode() != before.Mode() {
					t.Fatal("external fake device changed")
				}
			})
		}
	}
}

type containedHandoffProvider struct{ ports.AgentAccountProviderCLI }

func (*containedHandoffProvider) Identity(_ context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	if home == "" {
		return domain.AgentAccountIdentityCheck{}, nil
	}
	return domain.AgentAccountIdentityCheck{LoggedIn: true, SignedIn: true, Email: filepath.Base(home) + "@example.invalid"}, nil
}
func (*containedHandoffProvider) Limits(context.Context, string) (domain.AgentAccountLimits, error) {
	return domain.AgentAccountLimits{}, nil
}
func (*containedHandoffProvider) Logout(context.Context, string) error {
	return errors.New("native logout must not run during a protected handoff")
}

type containedHistoryAgent struct {
	transitionAgent
	envKey, historyDir string
	beforeNativeProbe  func(string)
}

func (a containedHistoryAgent) NativeConversationExists(_ context.Context, _ ports.SessionRef, id string, env map[string]string) (bool, error) {
	if a.beforeNativeProbe != nil {
		a.beforeNativeProbe(env[a.envKey])
	}
	data, err := os.ReadFile(filepath.Join(env[a.envKey], a.historyDir, id))
	return string(data) == "same-native-conversation", err
}
