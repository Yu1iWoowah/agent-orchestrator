package sessionmanager

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type accountBoundModelCatalog struct {
	t   *testing.T
	log *[]string
}

func (c accountBoundModelCatalog) Models(ctx context.Context, _ string, _ string, _ bool) (ports.AgentModelCatalog, error) {
	selected, ok := ports.AgentAccountFromContext(ctx)
	if !ok || selected.AccountID != "account-A" {
		c.t.Error("model lookup did not receive selected account")
	}
	bound := false
	for _, entry := range *c.log {
		if strings.HasPrefix(entry, "bind:account-A") {
			bound = true
		}
	}
	if !bound {
		c.t.Error("model discovery ran before durable account binding")
	}
	return ports.AgentModelCatalog{Models: []ports.AgentModelInfo{{ID: "native", IsDefault: true, Efforts: []string{"high"}}}}, nil
}
func TestSpawnModelsUseDurableAccountBinding(t *testing.T) {
	st, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	project := domain.ProjectRecord{ID: "fake", Path: t.TempDir(), RegisteredAt: time.Now(), Config: domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessClaudeCode}, AgentConfig: domain.AgentConfig{Model: "native"}}}
	if err := st.UpsertProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	m := New(Deps{Runtime: &fakeRuntime{}, Agents: singleAgent{agent: &recordingAgent{}}, Workspace: &fakeWorkspace{}, Store: st, Messenger: &fakeMessenger{}, Lifecycle: lifecycle.New(st, nil), LookPath: func(string) (string, error) { return "/bin/true", nil }})
	m.runBackground = func(work func()) { work() }
	log := []string{}
	m.SetAgentAccounts(&spawnBoundAccounts{transitionAccounts: newTransitionAccounts(&log), store: st})
	m.SetModelCatalog(accountBoundModelCatalog{t: t, log: &log})
	if _, _, _, err := m.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "fake", Kind: domain.KindWorker, AgentAccountID: "account-A"}); err != nil {
		t.Fatal(err)
	}
}
