package sessionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// This is a fake U3 public launch interface, not a managed-account integration.
type spawnBoundAccounts struct {
	*transitionAccounts
	store *sqlite.Store
}

type spawnCatalogFingerprint struct {
	current string
	calls   int
}

func (f *spawnCatalogFingerprint) CatalogFingerprint(context.Context, string, string) (string, error) {
	f.calls++
	return f.current, nil
}

func (a *spawnBoundAccounts) BindSession(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, account string) error {
	if err := a.transitionAccounts.BindSession(ctx, id, harness, account); err != nil {
		return err
	}
	provider, _ := domain.AgentAccountProviderFor(harness)
	return a.store.BindSessionAgentAccount(ctx, id, provider, account, time.Now())
}

func TestPreparedSpawnSettingsWithBoundAccount(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		for _, strict := range []bool{false, true} {
			name := string(harness) + "/legacy"
			if strict {
				name = string(harness) + "/strict"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				now := time.Now()
				st, err := sqlite.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close() })
				project := domain.ProjectRecord{ID: "fake", Path: t.TempDir(), RegisteredAt: now, Config: domain.ProjectConfig{Worker: domain.RoleOverride{Harness: harness}}}
				if err = st.UpsertProject(ctx, project); err != nil {
					t.Fatal(err)
				}
				catalog := ports.AgentModelCatalog{AgentID: string(harness), Models: []ports.AgentModelInfo{{ID: "native", IsDefault: true, SupportsFast: true}}}
				raw, _ := json.Marshal(catalog)
				if err = st.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: string(harness), CatalogJSON: string(raw), FetchedAt: now, LastSuccessAt: now, InputFingerprint: "prepared-inputs"}); err != nil {
					t.Fatal(err)
				}
				agent, rt, ws := &recordingAgent{}, &fakeRuntime{}, &fakeWorkspace{}
				m := New(Deps{Runtime: rt, Agents: singleAgent{agent: agent}, Workspace: ws, Store: st, Messenger: &fakeMessenger{}, Lifecycle: lifecycle.New(st, nil), LookPath: func(string) (string, error) { return "/bin/true", nil }})
				m.runBackground = func(work func()) { work() }
				log := []string{}
				accounts := &spawnBoundAccounts{transitionAccounts: newTransitionAccounts(&log), store: st}
				m.SetAgentAccounts(accounts)
				reader := agentsettings.New(st, func() time.Time { return now })
				reader.SetCatalogFingerprintSource(&spawnCatalogFingerprint{current: "prepared-inputs"})
				m.SetAgentSettingsPreflight(reader)
				token, err := m.PrepareTaskWorkspace(ctx, project)
				if err != nil || token == "" {
					t.Fatalf("prepare: token=%q err=%v", token, err)
				}
				provider, _ := domain.AgentAccountProviderFor(harness)
				account := "device-" + string(provider)
				cfg := ports.SpawnConfig{ProjectID: "fake", Kind: domain.KindWorker, TaskPreparation: token, AgentAccountID: account}
				if strict {
					on := domain.AgentFastOn
					cfg.AgentSettings = &domain.AgentSettingsPatch{FastMode: &on}
				}
				rec, _, _, err := m.Spawn(ctx, cfg)
				if err != nil {
					t.Fatalf("spawn with prepared workspace and bound account: %v", err)
				}
				loaded, found, err := st.GetSession(ctx, rec.ID)
				if err != nil || !found || loaded.IsTaskPreparation || loaded.Harness != harness || rec.ID != domain.SessionID(token) || ws.createCount != 1 || ws.destroyed != 0 || rt.created != 1 {
					t.Fatalf("session=%+v found=%v err=%v workspaces=%d destroyed=%d runtime=%d", loaded, found, err, ws.createCount, ws.destroyed, rt.created)
				}
				bound, err := accounts.SessionAccountID(ctx, rec.ID, harness)
				resolution := loaded.Metadata.SpawnAgentSettings
				if err != nil || bound != account || resolution == nil || resolution.AccountID != account || agent.lastConfig.Launch.FastMode != strict {
					t.Fatalf("bound=%q resolution=%+v launch=%+v err=%v", bound, resolution, agent.lastConfig, err)
				}
			})
		}
	}
}

func TestSpawnSettingsIntegrationRejectsUnqualifiedEvidenceBeforeLaunch(t *testing.T) {
	for _, tc := range []struct {
		name, account       string
		supportsFast, stale bool
		want                error
		currentFingerprint  string
		legacy              bool
	}{
		{name: "device qualified", account: "device-codex", supportsFast: true},
		{name: "device unknown", account: "device-codex", want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "device stale", account: "device-codex", supportsFast: true, stale: true, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "managed cannot borrow device", account: "managed", supportsFast: true, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "device identity changed", account: "device-codex", supportsFast: true, currentFingerprint: "binary-1:identity-B", want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "binary changed", account: "device-codex", supportsFast: true, currentFingerprint: "binary-2:identity-A", want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "legacy identity changed", account: "device-codex", supportsFast: true, currentFingerprint: "binary-1:identity-B", legacy: true},
		{name: "legacy binary changed", account: "device-codex", supportsFast: true, currentFingerprint: "binary-2:identity-A", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			st, err := sqlite.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err = st.UpsertProject(ctx, domain.ProjectRecord{ID: "fake", Path: t.TempDir(), RegisteredAt: now, Config: domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessCodex}}}); err != nil {
				t.Fatal(err)
			}
			catalog := ports.AgentModelCatalog{AgentID: "codex", Models: []ports.AgentModelInfo{{ID: "native", IsDefault: true, SupportsFast: tc.supportsFast}}}
			raw, _ := json.Marshal(catalog)
			observed := now
			if tc.stale {
				observed = now.AddDate(0, 0, -1)
			}
			if err = st.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), FetchedAt: observed, LastSuccessAt: observed, InputFingerprint: "binary-1:identity-A"}); err != nil {
				t.Fatal(err)
			}
			agent := &recordingAgent{}
			rt := &fakeRuntime{}
			ws := &fakeWorkspace{}
			m := New(Deps{Runtime: rt, Agents: singleAgent{agent: agent}, Workspace: ws, Store: st, Messenger: &fakeMessenger{}, Lifecycle: lifecycle.New(st, nil), LookPath: func(string) (string, error) { return "/bin/true", nil }})
			log := []string{}
			m.SetAgentAccounts(&spawnBoundAccounts{transitionAccounts: newTransitionAccounts(&log), store: st})
			reader := agentsettings.New(st, func() time.Time { return now })
			inputs := &spawnCatalogFingerprint{current: tc.currentFingerprint}
			if inputs.current == "" {
				inputs.current = "binary-1:identity-A"
			}
			reader.SetCatalogFingerprintSource(inputs)
			m.SetAgentSettingsPreflight(reader)
			on := domain.AgentFastOn
			cfg := ports.SpawnConfig{ProjectID: "fake", Kind: domain.KindWorker, AgentAccountID: tc.account}
			if !tc.legacy {
				cfg.AgentSettings = &domain.AgentSettingsPatch{FastMode: &on}
			}
			rec, _, _, err := m.Spawn(ctx, cfg)
			if tc.want != nil {
				if !errors.Is(err, tc.want) || ws.createCount != 0 || rt.created != 0 {
					t.Fatalf("err=%v workspace=%d runtime=%d", err, ws.createCount, rt.created)
				}
				rows, listErr := st.ListAllSessions(ctx)
				if listErr != nil || len(rows) != 0 {
					t.Fatalf("rejected seed remained: %v %v", rows, listErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			loaded, found, err := st.GetSession(ctx, rec.ID)
			if err != nil || !found || agent.lastConfig.Launch.FastMode == tc.legacy || !tc.legacy && loaded.Metadata.FastMode != on || loaded.Metadata.SpawnAgentSettings == nil || loaded.Metadata.SpawnAgentSettings.AccountID != tc.account {
				t.Fatalf("rec=%+v launch=%+v err=%v", loaded.Metadata, agent.lastConfig, err)
			}
			launch, err := st.SessionLaunchOptions(ctx, rec.ID)
			if err != nil || launch.FastMode == tc.legacy || launch.FastModeOff || tc.legacy && inputs.calls != 0 {
				t.Fatalf("projection=%+v err=%v", launch, err)
			}
		})
	}
}
