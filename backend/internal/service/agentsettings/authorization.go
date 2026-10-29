package agentsettings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Caller contains untrusted transport identity, not audit attribution. A
// malformed identity-bearing request must never fall back to local authority.
type Caller = ports.AgentSettingsCaller

// Authorization is the non-secret scope accepted at the service boundary.
type Authorization struct {
	Actor     domain.AgentSettingsActor
	Authority domain.AgentSettingsAuthority
}

// AuthorityStore reads actual actor role and generation, not client claims.
type AuthorityStore interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
}

// Authorizer confines settings-purpose grants to same-project workers.
type Authorizer struct {
	store  AuthorityStore
	grants *GrantAuthority
	now    func() time.Time
}

func NewAuthorizer(store AuthorityStore, grants *GrantAuthority, now func() time.Time) *Authorizer {
	if now == nil {
		now = time.Now
	}
	return &Authorizer{store: store, grants: grants, now: now}
}

// Authorize authenticates a transport caller. Policy is checked separately so
// revoking a selection does not prevent reading or cancelling its queued intent.
func (a *Authorizer) Authorize(ctx context.Context, caller Caller, target domain.SessionRecord) (Authorization, error) {
	if caller.SessionID == "" && caller.Grant == "" {
		return Authorization{Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}}, nil
	}
	id, scope, err := a.grants.Verify(caller.Grant)
	if err != nil || caller.SessionID != "" && id != caller.SessionID {
		code := "AGENT_SETTINGS_GRANT_INVALID"
		if errors.Is(err, ports.ErrAgentSettingsGrantExpired) {
			code = "AGENT_SETTINGS_GRANT_EXPIRED"
		}
		return Authorization{}, apierr.Forbidden(code, "A current settings-purpose grant is required")
	}
	accepted := Authorization{Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorOrchestrator, SessionID: id}, Authority: scope}
	if err := a.Revalidate(ctx, accepted, target); err != nil {
		return Authorization{}, err
	}
	return accepted, nil
}

// Revalidate checks accepted scope again before apply. This is not an entry
// point for untrusted callers; only Authorize may construct accepted scope.
func (a *Authorizer) Revalidate(ctx context.Context, accepted Authorization, target domain.SessionRecord) error {
	if accepted.Actor.Kind == domain.AgentSettingsActorLocal && accepted.Actor.SessionID == "" && accepted.Authority == (domain.AgentSettingsAuthority{}) {
		return nil
	}
	if accepted.Actor.Kind != domain.AgentSettingsActorOrchestrator || !validGrantScope(accepted.Actor.SessionID, accepted.Authority) || accepted.Authority.IssuedAt > a.now().Unix() {
		return apierr.Forbidden("AGENT_SETTINGS_ACTOR_DENIED", "The settings actor is no longer valid")
	}
	if accepted.Authority.ExpiresAt <= a.now().Unix() {
		return apierr.Forbidden("AGENT_SETTINGS_GRANT_EXPIRED", "The settings grant has expired")
	}
	rec, ok, err := a.store.GetSession(ctx, accepted.Actor.SessionID)
	if err != nil {
		return err
	}
	if !ok || rec.Kind != domain.KindOrchestrator || !accepted.Authority.Matches(rec) {
		return apierr.Forbidden("AGENT_SETTINGS_ACTOR_DENIED", "The current orchestrator owner is required")
	}
	sessions, err := a.store.ListSessions(ctx, rec.ProjectID)
	if err != nil {
		return err
	}
	for _, other := range sessions {
		if other.ProjectID == rec.ProjectID && other.Kind == domain.KindOrchestrator && !other.IsTerminated && other.ID != rec.ID {
			return apierr.Forbidden("AGENT_SETTINGS_ACTOR_DENIED", "The orchestrator owner is ambiguous")
		}
	}
	if target.ID == rec.ID || target.Kind != domain.KindWorker || target.ProjectID != rec.ProjectID || target.ProjectID == "" {
		return apierr.Forbidden("AGENT_SETTINGS_TARGET_DENIED", "Managed settings target must be another worker in the same project")
	}
	return nil
}

// CheckSelection returns a stable policy revision for runtime qualification.
// Manual local calls deliberately retain their pre-existing target range and
// exemption from project policy; they still require CAS/provider validation.
func (a *Authorizer) CheckSelection(ctx context.Context, accepted Authorization, target domain.SessionRecord, settings domain.AgentSettings) (string, error) {
	if err := a.Revalidate(ctx, accepted, target); err != nil {
		return "", err
	}
	if accepted.Actor.Kind == domain.AgentSettingsActorLocal {
		return "local_operator_unverified", nil
	}
	p, ok, err := a.store.GetProject(ctx, string(target.ProjectID))
	if err != nil {
		return "", err
	}
	policy := p.Config.AgentSettingsPolicy
	if !ok || p.ID != string(target.ProjectID) || policy == nil || policy.Validate() != nil || !policy.Allows(target.Harness, settings) {
		return "", apierr.Forbidden("AGENT_SETTINGS_POLICY_DENIED", "Project settings policy does not allow this selection")
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
