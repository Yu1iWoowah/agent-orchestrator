package agentsettings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReviewerAuthorizationNeedsItsOwnPolicyAndStableProjectOwner(t *testing.T) {
	for _, name := range []string{"worker policy only", "reviewer permitted", "reviewer own harness", "cross project", "wrong parent", "replaced actor", "invalid grant", "local compatibility"} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
			grants, err := NewGrantAuthority([]byte(strings.Repeat("r", 32)), func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			scope := domain.AgentSettingsAuthority{ProjectID: "p", ActorHarness: domain.HarnessCodex, ActorMode: domain.SessionModeChat, ActorGeneration: "generation", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
			grant, err := grants.Issue("orch", scope)
			if err != nil {
				t.Fatal(err)
			}
			allow := domain.AgentSettingsAllowance{Models: []domain.AgentModelSelection{"review-model"}, Efforts: []domain.AgentEffortSelection{"high"}, AllowFast: true}
			policy := &domain.AgentSettingsPolicy{Enabled: true, Harnesses: map[domain.AgentHarness]domain.AgentSettingsAllowance{domain.HarnessCodex: allow}}
			store := &authorityStore{actor: domain.SessionRecord{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{ControllerGeneration: "generation"}}, project: domain.ProjectRecord{ID: "p", Config: domain.ProjectConfig{AgentSettingsPolicy: policy}}}
			parent := domain.SessionRecord{ID: "worker", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex}
			review := domain.Review{ID: "review-owner", SessionID: parent.ID, ProjectID: parent.ProjectID, Harness: domain.ReviewerCodex}
			caller := Caller{SessionID: "orch", Grant: grant}
			want := ""
			switch name {
			case "worker policy only":
				want = "AGENT_SETTINGS_POLICY_DENIED"
			case "cross project":
				review.ProjectID, want = "other", "AGENT_SETTINGS_TARGET_DENIED"
			case "wrong parent":
				review.SessionID, want = "other-worker", "AGENT_SETTINGS_TARGET_DENIED"
			case "replaced actor":
				store.actor.Metadata.ControllerGeneration, want = "replacement", "AGENT_SETTINGS_ACTOR_DENIED"
			case "invalid grant":
				caller.Grant, want = "invalid", "AGENT_SETTINGS_GRANT_INVALID"
			case "reviewer own harness":
				review.Harness = domain.ReviewerClaudeCode
			case "local compatibility":
				caller, store.project.Config.AgentSettingsPolicy = Caller{}, nil
			}
			if name != "worker policy only" {
				policy.Reviewers = map[domain.ReviewerHarness]domain.AgentSettingsAllowance{review.Harness: allow}
			}
			auth, ok := any(NewAuthorizer(store, grants, func() time.Time { return now })).(ports.ReviewerSettingsAuthorizer)
			if !ok {
				t.Fatal("reviewer settings authorization is unavailable")
			}
			ctx := ports.WithAgentSettingsCaller(context.Background(), caller)
			accepted, err := auth.AuthorizeReviewerSelection(ctx, parent, review, domain.AgentSettings{Model: "review-model", Effort: "high", FastMode: domain.AgentFastOn})
			if want != "" {
				var wire *apierr.Error
				if !errors.As(err, &wire) || wire.Code != want {
					t.Fatalf("authorization = %v, want %s", err, want)
				}
				return
			}
			if err != nil || accepted.PolicyRevision == "" {
				t.Fatalf("authorized scope = %+v, %v", accepted, err)
			}
			if name == "local compatibility" && (accepted.Actor.Kind != domain.AgentSettingsActorLocal || store.projectReads != 0) {
				t.Fatal("local unverified request was relabelled or consulted managed policy")
			}
		})
	}
}
