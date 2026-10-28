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

func (a *spawnBoundAccounts) BindSession(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, account string) error {
	if err := a.transitionAccounts.BindSession(ctx, id, harness, account); err != nil {
		return err
	}
	return a.store.BindSessionAgentAccount(ctx, id, domain.AgentAccountProviderCodex, account, time.Now())
}

func TestSpawnSettingsIntegrationRejectsUnqualifiedEvidenceBeforeLaunch(t *testing.T) {
	for _, tc := range []struct {
		name, account       string
		supportsFast, stale bool
		want                error
	}{
		{"device qualified", "device-codex", true, false, nil},
		{"device unknown", "device-codex", false, false, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"device stale", "device-codex", true, true, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"managed cannot borrow device", "managed", true, false, ports.ErrAgentSettingsCapabilitiesUnavailable},
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
			if err = st.UpsertAgentModelCatalog(ctx, ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), FetchedAt: observed, LastSuccessAt: observed}); err != nil {
				t.Fatal(err)
			}
			agent := &recordingAgent{}
			rt := &fakeRuntime{}
			ws := &fakeWorkspace{}
			m := New(Deps{Runtime: rt, Agents: singleAgent{agent: agent}, Workspace: ws, Store: st, Messenger: &fakeMessenger{}, Lifecycle: lifecycle.New(st, nil), LookPath: func(string) (string, error) { return "/bin/true", nil }})
			log := []string{}
			m.SetAgentAccounts(&spawnBoundAccounts{transitionAccounts: newTransitionAccounts(&log), store: st})
			m.SetAgentSettingsPreflight(agentsettings.New(st, func() time.Time { return now }))
			on := domain.AgentFastOn
			rec, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: "fake", Kind: domain.KindWorker, AgentAccountID: tc.account, AgentSettings: &domain.AgentSettingsPatch{FastMode: &on}})
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
			if err != nil || !found || !agent.lastConfig.Launch.FastMode || loaded.Metadata.FastMode != on || loaded.Metadata.SpawnAgentSettings == nil || loaded.Metadata.SpawnAgentSettings.AccountID != tc.account {
				t.Fatalf("rec=%+v launch=%+v err=%v", loaded.Metadata, agent.lastConfig, err)
			}
			launch, err := st.SessionLaunchOptions(ctx, rec.ID)
			if err != nil || !launch.FastMode || launch.FastModeOff {
				t.Fatalf("projection=%+v err=%v", launch, err)
			}
		})
	}
}
