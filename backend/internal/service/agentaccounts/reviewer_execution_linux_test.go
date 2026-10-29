package agentaccounts

import (
	"context"
	"errors"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewerRebuildPreservesBindingsAndFencesStaleLaunch(t *testing.T) {
	for _, provider := range domain.AgentAccountProviders {
		t.Run(string(provider), func(t *testing.T) {
			ctx := context.Background()
			db := sqlitetest.MustOpen(t)
			now := time.Now().UTC()
			if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "fake-project", Path: t.TempDir(), RegisteredAt: now}); err != nil {
				t.Fatal(err)
			}
			harness := domain.HarnessClaudeCode
			if provider == domain.AgentAccountProviderCodex {
				harness = domain.HarnessCodex
			}
			worker, err := db.CreateSession(ctx, domain.SessionRecord{ID: "fake-worker", ProjectID: "fake-project", Harness: harness, Kind: domain.KindWorker, Mode: domain.SessionModeTUI, CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "accounts")
			cli := &fakeClaude{signedInAs: map[string]string{}}
			var native ports.AgentAccountProviderCLI = cli
			if provider == domain.AgentAccountProviderCodex {
				native = &codexReviewerFixture{fakeClaude: cli}
			}
			svc := New(db, []ports.AgentAccountProviderCLI{native}, root, nil, nil)
			for _, id := range []string{"A", "B", "D"} {
				home := filepath.Join(root, string(provider), id)
				if err := ensureAccountProfile(root, home); err != nil {
					t.Fatal(err)
				}
				if provider == domain.AgentAccountProviderCodex {
					if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("fake"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				email := id + "@example.test"
				cli.signedInAs[home] = email
				if err := db.CreateAgentAccount(ctx, domain.AgentAccount{ID: id, Provider: provider, Home: home, Email: email, CreatedAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			old := domain.Review{ID: "old", SessionID: worker.ID, ProjectID: "fake-project", Harness: domain.ReviewerHarness(harness), AgentSessionID: "original-native", CreatedAt: now, UpdatedAt: now}
			if err := db.UpsertReview(ctx, old); err != nil {
				t.Fatal(err)
			}
			request := ports.AgentAccountResolutionRequest{Harness: harness, Role: domain.AgentAccountRoleReviewer, AccountID: "A"}
			if _, err := svc.ResolveReviewerAccount(ctx, old.ID, request); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.RebuildReviewer(ctx, old.ID, "B"); !errors.Is(err, ports.ErrAgentAccountInUse) {
				t.Fatalf("live owner replaced: %v", err)
			}
			if err := db.ArchiveReviewsBySession(ctx, old.SessionID, now); err != nil {
				t.Fatal(err)
			}
			fresh, err := svc.RebuildReviewer(ctx, old.ID, "B")
			if err != nil {
				t.Fatal(err)
			}
			if fresh.ID == old.ID || fresh.AgentSessionID != "" {
				t.Fatal("reused native execution")
			}
			if err := db.SetAgentAccountDefault(ctx, provider, "D"); err != nil {
				t.Fatal(err)
			}
			request.AccountID = ""
			selected, err := svc.ResolveReviewerAccount(ctx, fresh.ID, request)
			if err != nil || selected.AccountID != "B" {
				t.Fatalf("next pass lost fixed identity: %+v %v", selected, err)
			}
			env, err := svc.ReviewerLaunchEnv(ctx, fresh.ID, harness)
			if err != nil || env[nativeHomeKey(provider)] != filepath.Join(root, string(provider), "B") {
				t.Fatalf("new pass wrong env: %v %v", env, err)
			}
			if _, err := svc.ResolveReviewerAccount(ctx, old.ID, request); !errors.Is(err, ports.ErrAgentAccountChanged) {
				t.Fatalf("stale trigger accepted: %v", err)
			}
			if _, err := svc.ReviewerLaunchEnv(ctx, old.ID, harness); !errors.Is(err, ports.ErrAgentAccountChanged) {
				t.Fatalf("stale native launch accepted: %v", err)
			}
			binding, _, err := db.GetReviewerAgentAccount(ctx, old.ID, provider)
			if err != nil || binding.AccountID != "A" {
				t.Fatal("historical binding mutated", binding, err)
			}
		})
	}
}

type codexReviewerFixture struct{ *fakeClaude }

func (*codexReviewerFixture) Provider() domain.AgentAccountProvider {
	return domain.AgentAccountProviderCodex
}
func (*codexReviewerFixture) LaunchEnv(home string) map[string]string {
	return map[string]string{"CODEX_HOME": home, "AO_CODEX_MANAGED_ACCOUNT": "1"}
}
func nativeHomeKey(p domain.AgentAccountProvider) string {
	if p == domain.AgentAccountProviderCodex {
		return "CODEX_HOME"
	}
	return "CLAUDE_CONFIG_DIR"
}
