package agentsettings

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// GrantEnvironment is a daemon-only bootstrap. No API returns these bearers.
type GrantEnvironment struct {
	store  AuthorityStore
	signer *GrantAuthority
	now    func() time.Time
}

func NewGrantEnvironment(store AuthorityStore, signer *GrantAuthority, now func() time.Time) *GrantEnvironment {
	if now == nil {
		now = time.Now
	}
	return &GrantEnvironment{store: store, signer: signer, now: now}
}

func (g *GrantEnvironment) AgentSettingsGrantEnv(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, mode domain.SessionMode, generation string) (map[string]string, error) {
	env := map[string]string{ports.EnvAgentSettingsGrant: ""}
	rec, ok, err := g.store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok || rec.Kind != domain.KindOrchestrator {
		return env, nil
	}
	if rec.ProjectID == "" {
		return env, nil
	}
	// Native restore starts before the durable owner claim. This provisional
	// credential cannot authorize until that claim: Authorizer checks the
	// current role, liveness, sole orchestrator and exact generation on every use.
	now := g.now()
	token, err := g.signer.Issue(id, domain.AgentSettingsAuthority{ProjectID: rec.ProjectID, ActorHarness: harness, ActorMode: domain.NormalizeSessionMode(mode), ActorGeneration: generation, IssuedAt: now.Unix(), ExpiresAt: now.Add(SettingsGrantLifetime).Unix()})
	if err != nil {
		return nil, err
	}
	env[ports.EnvAgentSettingsGrant] = token
	return env, nil
}
