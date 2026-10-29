package agentsettings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type authorityStore struct {
	actor        domain.SessionRecord
	others       []domain.SessionRecord
	project      domain.ProjectRecord
	projectReads int
}

func (s *authorityStore) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	return s.actor, id == s.actor.ID, nil
}
func (s *authorityStore) ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error) {
	return append([]domain.SessionRecord{s.actor}, s.others...), nil
}
func (s *authorityStore) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	s.projectReads++
	return s.project, true, nil
}

func TestSettingsManagedAuthorizationChecksCurrentActorTargetAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*authorityStore, *domain.SessionRecord, *Caller, *time.Time)
		code   string
	}{
		{name: "accepted"},
		{"worker impersonation", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			s.actor.Kind = domain.KindWorker
		}, "AGENT_SETTINGS_ACTOR_DENIED"},
		{"terminated actor", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) { s.actor.IsTerminated = true }, "AGENT_SETTINGS_ACTOR_DENIED"},
		{"rotated generation", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			s.actor.Metadata.ControllerGeneration = "new"
		}, "AGENT_SETTINGS_ACTOR_DENIED"},
		{"changed actor project", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) { s.actor.ProjectID = "other" }, "AGENT_SETTINGS_ACTOR_DENIED"},
		{"changed harness", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			s.actor.Harness = domain.HarnessClaudeCode
		}, "AGENT_SETTINGS_ACTOR_DENIED"},
		{"changed mode", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			s.actor.Mode = domain.SessionModeTUI
		}, "AGENT_SETTINGS_ACTOR_DENIED"},
		{"ambiguous orchestrator", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			s.others = []domain.SessionRecord{{ID: "second", ProjectID: "p", Kind: domain.KindOrchestrator}}
		}, "AGENT_SETTINGS_ACTOR_DENIED"},
		{"expired", func(_ *authorityStore, _ *domain.SessionRecord, _ *Caller, n *time.Time) { *n = n.Add(time.Hour) }, "AGENT_SETTINGS_GRANT_EXPIRED"},
		{"forged grant", func(_ *authorityStore, _ *domain.SessionRecord, c *Caller, _ *time.Time) { c.Grant = "forged" }, "AGENT_SETTINGS_GRANT_INVALID"},
		{"claim only", func(_ *authorityStore, _ *domain.SessionRecord, c *Caller, _ *time.Time) { c.Grant = "" }, "AGENT_SETTINGS_GRANT_INVALID"},
		{"mismatched claim", func(_ *authorityStore, _ *domain.SessionRecord, c *Caller, _ *time.Time) { c.SessionID = "worker" }, "AGENT_SETTINGS_GRANT_INVALID"},
		{"self", func(s *authorityStore, r *domain.SessionRecord, _ *Caller, _ *time.Time) { *r = s.actor }, "AGENT_SETTINGS_TARGET_DENIED"},
		{"orchestrator target", func(_ *authorityStore, r *domain.SessionRecord, _ *Caller, _ *time.Time) {
			r.Kind = domain.KindOrchestrator
		}, "AGENT_SETTINGS_TARGET_DENIED"},
		{"cross project", func(_ *authorityStore, r *domain.SessionRecord, _ *Caller, _ *time.Time) { r.ProjectID = "other" }, "AGENT_SETTINGS_TARGET_DENIED"},
		{"standalone", func(_ *authorityStore, r *domain.SessionRecord, _ *Caller, _ *time.Time) { r.ProjectID = "" }, "AGENT_SETTINGS_TARGET_DENIED"},
		{"policy absent", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			s.project.Config.AgentSettingsPolicy = nil
		}, "AGENT_SETTINGS_POLICY_DENIED"},
		{"policy disabled", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			s.project.Config.AgentSettingsPolicy.Enabled = false
		}, "AGENT_SETTINGS_POLICY_DENIED"},
		{"model denied", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			a := s.project.Config.AgentSettingsPolicy.Harnesses[domain.HarnessCodex]
			a.Models = nil
			s.project.Config.AgentSettingsPolicy.Harnesses[domain.HarnessCodex] = a
		}, "AGENT_SETTINGS_POLICY_DENIED"},
		{"effort denied", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			a := s.project.Config.AgentSettingsPolicy.Harnesses[domain.HarnessCodex]
			a.Efforts = nil
			s.project.Config.AgentSettingsPolicy.Harnesses[domain.HarnessCodex] = a
		}, "AGENT_SETTINGS_POLICY_DENIED"},
		{"fast denied", func(s *authorityStore, _ *domain.SessionRecord, _ *Caller, _ *time.Time) {
			a := s.project.Config.AgentSettingsPolicy.Harnesses[domain.HarnessCodex]
			a.AllowFast = false
			s.project.Config.AgentSettingsPolicy.Harnesses[domain.HarnessCodex] = a
		}, "AGENT_SETTINGS_POLICY_DENIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			a, _ := NewGrantAuthority([]byte(strings.Repeat("k", 32)), func() time.Time { return now })
			scope := domain.AgentSettingsAuthority{ProjectID: "p", ActorHarness: domain.HarnessCodex, ActorMode: domain.SessionModeChat, ActorGeneration: "gen", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
			token, err := a.Issue("orch", scope)
			if err != nil {
				t.Fatal(err)
			}
			s := &authorityStore{actor: domain.SessionRecord{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{ControllerGeneration: "gen"}}, project: domain.ProjectRecord{ID: "p", Config: domain.ProjectConfig{AgentSettingsPolicy: &domain.AgentSettingsPolicy{Enabled: true, Harnesses: map[domain.AgentHarness]domain.AgentSettingsAllowance{domain.HarnessCodex: {Models: []domain.AgentModelSelection{"model"}, Efforts: []domain.AgentEffortSelection{"high"}, AllowFast: true}}}}}}
			target := domain.SessionRecord{ID: "worker", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessCodex}
			caller := Caller{SessionID: "orch", Grant: token}
			if tc.change != nil {
				tc.change(s, &target, &caller, &now)
			}
			auth := NewAuthorizer(s, a, func() time.Time { return now })
			accepted, err := auth.Authorize(context.Background(), caller, target)
			if err == nil {
				_, err = auth.CheckSelection(context.Background(), accepted, target, domain.AgentSettings{Model: "model", Effort: "high", FastMode: domain.AgentFastOn})
			}
			if tc.code != "" {
				var e *apierr.Error
				if !errors.As(err, &e) || e.Code != tc.code {
					t.Fatalf("want %s, got %v", tc.code, err)
				}
				return
			}
			if err != nil || accepted.Actor.Kind != domain.AgentSettingsActorOrchestrator || accepted.Authority != scope {
				t.Fatalf("accepted scope: %+v %v", accepted, err)
			}
		})
	}
}

func TestSettingsLocalAuthorityExemptsPolicyWithoutClaimingIdentity(t *testing.T) {
	s := &authorityStore{}
	a := NewAuthorizer(s, nil, time.Now)
	for _, target := range []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator}, {ID: "standalone", Kind: domain.KindWorker}, {ID: "other", ProjectID: "other", Kind: domain.KindWorker}} {
		got, err := a.Authorize(context.Background(), Caller{}, target)
		if err != nil || got.Actor != (domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}) || got.Authority != (domain.AgentSettingsAuthority{}) {
			t.Fatalf("local scope: %+v %v", got, err)
		}
		if _, err := a.CheckSelection(context.Background(), got, target, domain.AgentSettings{FastMode: domain.AgentFastOn}); err != nil {
			t.Fatal(err)
		}
	}
	if s.projectReads != 0 {
		t.Fatal("local request consulted managed project policy")
	}
	if _, err := a.Authorize(context.Background(), Caller{Grant: "invalid"}, domain.SessionRecord{}); err == nil {
		t.Fatal("identity-bearing request downgraded to local")
	}
}
