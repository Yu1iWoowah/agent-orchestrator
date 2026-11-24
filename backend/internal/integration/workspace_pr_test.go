package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	scmgithub "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/github"
	scmmulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/scm/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
)

func TestWorkspacePRDiscoveryAndClaimPersist(t *testing.T) {
	ctx := context.Background()
	f := newSCMFixture(t, "ao/octo-1-2")
	project := domain.ProjectRecord{ID: "octo", Kind: domain.ProjectKindWorkspace, Path: t.TempDir(), RegisteredAt: f.now}
	if err := f.store.UpsertWorkspaceProject(ctx, project, []domain.WorkspaceRepoRecord{{Name: "hello", RelativePath: "hello", RepoOriginURL: scmTestOriginURL, RegisteredAt: f.now}}); err != nil {
		t.Fatal(err)
	}
	// A repository registered to another workspace must not authorize a claim.
	if err := f.store.UpsertWorkspaceProject(ctx, domain.ProjectRecord{ID: "other", Kind: domain.ProjectKindWorkspace, Path: t.TempDir(), RegisteredAt: f.now}, []domain.WorkspaceRepoRecord{{Name: "hello", RelativePath: "hello", RepoOriginURL: "https://github.com/other/hello", RegisteredAt: f.now}}); err != nil {
		t.Fatal(err)
	}
	observed := failingSCMObservation("https://github.com/octocat/hello/pull/1", 1, "sha1", "build failed")
	observed.PR.SourceBranch = "ao/octo-1-2-fix"
	observed.PR.HeadRepo = "octocat/hello"
	f.provider.detected[observed.PR.SourceBranch] = observed.PR
	f.provider.observations[1] = observed
	if err := f.observer.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	prs, err := f.store.ListPRsBySession(ctx, f.session.ID)
	if err != nil || len(prs) != 1 || prs[0].URL != observed.PR.URL || prs[0].SourceBranch != observed.PR.SourceBranch {
		t.Fatalf("automatically attributed PRs = %+v, error = %v", prs, err)
	}

	claimed := failingSCMObservation("https://github.com/octocat/hello/pull/2", 2, "sha2", "build failed")
	claimed.PR.SourceBranch = "existing-custom-branch"
	f.provider.observations[2] = claimed
	svc := sessionsvc.NewWithDeps(sessionsvc.Deps{Store: f.store, SCM: f.provider, PRClaimer: f.store})
	result, err := svc.ClaimPR(ctx, f.session.ID, claimed.PR.URL, sessionsvc.ClaimPROptions{})
	if err != nil || len(result.PRs) != 2 || result.PRs[0].URL != claimed.PR.URL {
		t.Fatalf("claimed PRs = %+v, error = %v", result.PRs, err)
	}
	if _, err := svc.ClaimPR(ctx, f.session.ID, "https://github.com/other/hello/pull/2", sessionsvc.ClaimPROptions{}); !errors.Is(err, sessionsvc.ErrProjectMismatch) {
		t.Fatalf("claim across workspace registry = %v, want ErrProjectMismatch", err)
	}
	prs, err = f.store.ListPRsBySession(ctx, f.session.ID)
	if err != nil || len(prs) != 2 {
		t.Fatalf("persisted PRs = %+v, error = %v", prs, err)
	}
}

func TestClaimPRPersistsFactsWhenGitHubDetailsTimeOut(t *testing.T) {
	ctx := context.Background()
	f := newSCMFixture(t, "ao/octo-1-2")
	const prURL = "https://github.com/octocat/hello/pull/127"
	const headSHA = "0123456789abcdef0123456789abcdef01234567"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid GraphQL request", http.StatusBadRequest)
			return
		}
		if strings.Contains(request.Query, "commitList:") || strings.Contains(request.Query, "conversation:") ||
			strings.Contains(request.Query, "labels(") || strings.Contains(request.Query, "reviewRequests(") {
			t.Log("fake GitHub returned 504 for the PR display fields")
			http.Error(w, "PR display fields timed out", http.StatusGatewayTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.Query, "reviewThreads("):
			_, _ = io.WriteString(w, `{"data":{"repo":{"pullRequest":{
				"reviewDecision":"APPROVED",
				"reviewSummaries":{"nodes":[{"id":"review-1","state":"APPROVED","author":{"login":"reviewer"}}]}
			}}}}`)
		case strings.Contains(request.Query, "statusCheckRollup"):
			_, _ = fmt.Fprintf(w, `{"data":{"pr0":{"pullRequest":{
				"number":127,"url":%q,"state":"OPEN","headRefOid":%q,
				"commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS",
					"contexts":{"nodes":[{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS"}]}
				}}}]}
			}}}}`, prURL, headSHA)
		default:
			http.Error(w, "unexpected GraphQL query", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)

	github, err := scmgithub.NewProvider(scmgithub.ProviderOptions{HTTPClient: server.Client(), GraphQLURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	scm := scmmulti.New(scmmulti.NamedProvider{Key: "github", Provider: github})
	svc := sessionsvc.NewWithDeps(sessionsvc.Deps{Store: f.store, SCM: scm, PRClaimer: f.store})
	if _, err := svc.ClaimPR(ctx, f.session.ID, "127", sessionsvc.ClaimPROptions{}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	prs, err := f.store.ListPRsBySession(ctx, f.session.ID)
	if err != nil || len(prs) != 1 || prs[0].URL != prURL || prs[0].HeadSHA != headSHA ||
		prs[0].CI != domain.CIPassing || prs[0].Review != domain.ReviewApproved {
		t.Fatalf("persisted PR = %+v, error = %v", prs, err)
	}
	checks, err := f.store.ListChecks(ctx, prURL)
	if err != nil || len(checks) != 1 || checks[0].Name != "build" || checks[0].CommitHash != headSHA || checks[0].Conclusion != "success" {
		t.Fatalf("persisted CI checks = %+v, error = %v", checks, err)
	}
	reviews, err := f.store.ListPRReviews(ctx, prURL)
	if err != nil || len(reviews) != 1 || reviews[0].ID != "review-1" || reviews[0].State != domain.ReviewApproved {
		t.Fatalf("persisted reviews = %+v, error = %v", reviews, err)
	}
}
