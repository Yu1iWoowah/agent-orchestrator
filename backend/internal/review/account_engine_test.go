package review

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type savedReviewerModels struct {
	refreshes []bool
	catalog   ports.AgentModelCatalog
}

func (m *savedReviewerModels) ModelCatalog(_ context.Context, _, _ string, refresh bool) (ports.AgentModelCatalog, error) {
	m.refreshes = append(m.refreshes, refresh)
	if refresh {
		return ports.AgentModelCatalog{}, errors.New("native discovery unavailable")
	}
	return m.catalog, nil
}

func TestReviewerUsesSavedCatalogBeforeNativeDiscovery(t *testing.T) {
	for _, stale := range []bool{false, true} {
		models := &savedReviewerModels{catalog: ports.AgentModelCatalog{Stale: stale, Models: []ports.AgentModelInfo{{ID: "saved-model", Efforts: []string{"high"}}}}}
		engine := &Engine{models: models}
		err := engine.validateReviewerModels(context.Background(), domain.ReviewerCodex, domain.AgentConfig{Model: "saved-model", Effort: "high"})
		if !stale && (err != nil || len(models.refreshes) != 1 || models.refreshes[0]) {
			t.Fatalf("saved catalog triggered discovery: %v %v", models.refreshes, err)
		}
		if stale && (err == nil || len(models.refreshes) != 2 || models.refreshes[0] || !models.refreshes[1]) {
			t.Fatalf("stale catalog was not refreshed: %v %v", models.refreshes, err)
		}
	}
}

type recordingReviewBinding struct {
	ports.ReviewerAccountBindings
	owner   string
	request ports.AgentAccountResolutionRequest
}

func (b *recordingReviewBinding) ResolveReviewerAccount(_ context.Context, id string, in ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	b.owner = id
	b.request = in
	p, _ := domain.AgentAccountProviderFor(in.Harness)
	return domain.ResolvedAgentAccount{AccountID: in.ProjectConfig.RoleAccountReference(in.Role, in.Harness), Provider: p, Source: domain.AgentAccountSourceRole}, nil
}

type accountAwareReviewLauncher struct {
	*fakeLauncher
	t *testing.T
}

func (l accountAwareReviewLauncher) Preflight(ctx context.Context, _ domain.ReviewerHarness, _ string, _ ...domain.ReviewerInterfaceMode) error {
	selected, ok := ports.AgentAccountFromContext(ctx)
	if !ok || selected.AccountID != "reviewer-C" {
		l.t.Errorf("review preflight used worker/default: %+v", selected)
	}
	return nil
}
func TestEngineBindsReviewerExecutionBeforePreflight(t *testing.T) {
	worker := liveWorker()
	cfg := domain.ProjectConfig{Worker: domain.RoleOverride{AgentAccountID: "worker-A"}, Orchestrator: domain.RoleOverride{AgentAccountID: "orchestrator-B"}, Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode, AgentAccountID: "reviewer-C"}}}
	store := &fakeStore{}
	launcher := accountAwareReviewLauncher{fakeLauncher: &fakeLauncher{}, t: t}
	engine := newEngineForTest(store, fakeSessions{rec: worker, ok: true}, prAt("sha1"), fakeProjects{cfg: cfg}, launcher)
	accounts := &recordingReviewBinding{}
	engine.accounts = accounts
	if _, err := engine.TriggerWithOptions(context.Background(), worker.ID, TriggerOptions{}); err != nil {
		t.Fatal(err)
	}
	if store.review == nil || accounts.owner != store.review.ID || accounts.owner == string(worker.ID) {
		t.Fatalf("wrong binding owner: %s review=%+v", accounts.owner, store.review)
	}
	if accounts.request.Role != domain.AgentAccountRoleReviewer {
		t.Fatal("reviewer used worker role")
	}
}
