package domain_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementFingerprintSeparatesRegistryMetadataFromLaunchFacts(t *testing.T) {
	cases := []struct {
		name   string
		change func(*domain.ProjectRecord)
		same   bool
	}{
		{"display name", func(p *domain.ProjectRecord) { p.DisplayName = "renamed" }, true},
		{"registration timestamp", func(p *domain.ProjectRecord) { p.RegisteredAt = p.RegisteredAt.Add(time.Hour) }, true},
		{"path", func(p *domain.ProjectRecord) { p.Path = "/another/repo" }, false},
		{"origin", func(p *domain.ProjectRecord) { p.RepoOriginURL = "https://example.invalid/other/repo.git" }, false},
		{"kind", func(p *domain.ProjectRecord) { p.Kind = domain.ProjectKindWorkspace }, false},
		{"branch", func(p *domain.ProjectRecord) { p.Config.DefaultBranch = "another-base" }, false},
		{"environment", func(p *domain.ProjectRecord) { p.Config.Env = map[string]string{"ROLE": "different"} }, false},
		{"model", func(p *domain.ProjectRecord) { p.Config.AgentConfig.Model = "different-model" }, false},
		{"orchestrator rules", func(p *domain.ProjectRecord) { p.Config.OrchestratorRules = "different standing rules" }, false},
		{"handoff root", func(p *domain.ProjectRecord) { p.Config.OrchestratorHandoff.SharedDir = "new-living" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := domain.ProjectRecord{ID: "project", Path: "/repo", RepoOriginURL: "https://example.invalid/owner/repo.git", DisplayName: "original", RegisteredAt: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), Kind: domain.ProjectKindSingleRepo, Config: domain.DefaultProjectConfig()}
			before, err := domain.ReplacementProjectFingerprint(p)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&p)
			after, err := domain.ReplacementProjectFingerprint(p)
			if err != nil {
				t.Fatal(err)
			}
			if (before == after) != tc.same {
				t.Fatalf("%s fingerprint same=%t want=%t", tc.name, before == after, tc.same)
			}
		})
	}
}
