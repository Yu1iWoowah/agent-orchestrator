package agentsettings

import (
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSettingsGrantEnvironmentIsPurposeAndGenerationScoped(t *testing.T) {
	now := time.Now()
	signer, _ := NewGrantAuthority([]byte(strings.Repeat("k", 32)), func() time.Time { return now })
	s := &authorityStore{actor: domain.SessionRecord{ID: "actor", ProjectID: "p", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{ControllerGeneration: "old"}}}
	issuer := NewGrantEnvironment(s, signer, func() time.Time { return now })
	env, err := issuer.AgentSettingsGrantEnv(t.Context(), "actor", domain.HarnessCodex, domain.SessionModeChat, "next")
	if err != nil {
		t.Fatal(err)
	}
	token := env[ports.EnvAgentSettingsGrant]
	id, scope, err := signer.Verify(token)
	if err != nil || id != "actor" || scope.ActorGeneration != "next" || scope.ProjectID != "p" {
		t.Fatalf("scope %+v %v", scope, err)
	}
	if scope.Matches(s.actor) {
		t.Fatal("future launch credential matched previous generation")
	}
	s.actor.Metadata.ControllerGeneration = "next"
	if !scope.Matches(s.actor) {
		t.Fatal("committed generation cannot use grant")
	}
	s.actor.Metadata.ControllerGeneration = "replacement"
	if scope.Matches(s.actor) {
		t.Fatal("stale launch kept authority")
	}
	s.actor.Kind = domain.KindWorker
	env, err = issuer.AgentSettingsGrantEnv(t.Context(), "actor", domain.HarnessCodex, domain.SessionModeChat, "worker")
	if err != nil || env[ports.EnvAgentSettingsGrant] != "" {
		t.Fatal("worker inherited settings authority")
	}
}

func TestSettingsGrantBootstrapWaitsForDurableRestoreClaim(t *testing.T) {
	now := time.Now()
	signer, _ := NewGrantAuthority([]byte(strings.Repeat("p", 32)), func() time.Time { return now })
	s := &authorityStore{actor: domain.SessionRecord{ID: "restored", ProjectID: "p", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI, IsTerminated: true, Metadata: domain.SessionMetadata{RuntimeLaunchID: "old"}}}
	issuer := NewGrantEnvironment(s, signer, func() time.Time { return now })
	env, err := issuer.AgentSettingsGrantEnv(t.Context(), s.actor.ID, domain.HarnessCodex, domain.SessionModeTUI, "new")
	if err != nil {
		t.Fatal("restoring native process cannot prepare future credential:", err)
	}
	caller := Caller{SessionID: s.actor.ID, Grant: env[ports.EnvAgentSettingsGrant]}
	authorizer := NewAuthorizer(s, signer, func() time.Time { return now })
	target := domain.SessionRecord{ID: "worker", ProjectID: "p", Kind: domain.KindWorker}
	if _, err := authorizer.Authorize(t.Context(), caller, target); err == nil {
		t.Fatal("terminated owner used provisional grant")
	}
	s.actor.IsTerminated = false
	if _, err := authorizer.Authorize(t.Context(), caller, target); err == nil {
		t.Fatal("old generation used future grant")
	}
	s.actor.Metadata.RuntimeLaunchID = "new"
	s.others = []domain.SessionRecord{{ID: "other", ProjectID: "p", Kind: domain.KindOrchestrator}}
	if _, err := authorizer.Authorize(t.Context(), caller, target); err == nil {
		t.Fatal("ambiguous active owner used a grant")
	}
	s.others = nil
	if _, err := authorizer.Authorize(t.Context(), caller, target); err != nil {
		t.Fatal("committed sole owner lost its grant:", err)
	}
}
