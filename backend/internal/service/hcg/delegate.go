package hcg

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

func delegationPolicy(p domain.HCGDelegationPolicySnapshot) reporting.DelegationPolicySnapshot {
	out := reporting.DelegationPolicySnapshot{Project: reporting.DelegationProjectPolicy{Enabled: p.Project.Enabled}, Worker: p.Worker, Orchestrator: p.Orchestrator}
	for _, ref := range p.Project.AllowedProfiles {
		out.Project.AllowedProfiles = append(out.Project.AllowedProfiles, reporting.DelegationProfileRef{ID: ref.ID, Revision: ref.Revision})
	}
	if p.Project.SelectedProfile != nil {
		out.Project.SelectedProfile = &reporting.DelegationProfileRef{ID: p.Project.SelectedProfile.ID, Revision: p.Project.SelectedProfile.Revision}
	}
	return out
}

// DelegationProjectPolicyChanged publishes an ordered committed policy snapshot.
func (s *Service) DelegationProjectPolicyChanged(id domain.ProjectID, p domain.HCGDelegationPolicySnapshot) {
	if s.delegation != nil {
		s.delegation.ProjectPolicyChanged(string(id), delegationPolicy(p))
	}
}
func delegationUnavailable() error {
	return apierr.Unavailable("HCG_DELEGATION_UNAVAILABLE", "Fixed delegation integration is unavailable")
}
func (s *Service) delegationFacts(ctx context.Context, id domain.SessionID) (reporting.Identity, reporting.DelegationPolicySnapshot, error) {
	rec, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return reporting.Identity{}, reporting.DelegationPolicySnapshot{}, err
	}
	if !ok {
		return reporting.Identity{}, reporting.DelegationPolicySnapshot{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	i := s.identity(rec)
	project, ok, err := s.store.GetProject(ctx, string(rec.ProjectID))
	if err != nil {
		return i, reporting.DelegationPolicySnapshot{}, err
	}
	if !ok {
		return i, reporting.DelegationPolicySnapshot{}, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	return i, delegationPolicy(project.Config.DelegationPolicySnapshot()), nil
}

// GetDelegation reads current policy and original launch facts.
func (s *Service) GetDelegation(ctx context.Context, id domain.SessionID) (reporting.DelegationView, error) {
	i, p, err := s.delegationFacts(ctx, id)
	if err != nil {
		return reporting.DelegationView{}, err
	}
	if s.delegation == nil {
		return reporting.DelegationView{}, delegationUnavailable()
	}
	return s.delegation.Get(i, p), nil
}

// SetDelegationOverride sets or clears only the session boolean override.
func (s *Service) SetDelegationOverride(ctx context.Context, id domain.SessionID, enabled *bool) (reporting.DelegationView, error) {
	i, p, err := s.delegationFacts(ctx, id)
	if err != nil {
		return reporting.DelegationView{}, err
	}
	if s.delegation == nil {
		return reporting.DelegationView{}, delegationUnavailable()
	}
	if s.delegation.SetOverride(ctx, i, enabled) != nil {
		return reporting.DelegationView{}, delegationUnavailable()
	}
	return s.delegation.Get(i, p), nil
}

// DelegationEmergencyStop cancels delegation across every project.
func (s *Service) DelegationEmergencyStop(ctx context.Context, enabled bool) error {
	if s.delegation == nil || s.delegation.EmergencyStop(ctx, enabled) != nil {
		return delegationUnavailable()
	}
	return nil
}

// DelegationProfiles returns safe fixed host profile references.
func (s *Service) DelegationProfiles(context.Context) ([]reporting.DelegationResolvedProfile, error) {
	if s.delegation == nil {
		return nil, delegationUnavailable()
	}
	return s.delegation.Profiles(), nil
}

// RegisterDelegation registers an immutable approved descriptor.
func (s *Service) RegisterDelegation(ctx context.Context, r reporting.DelegationRegistration) ([]reporting.DelegationResolvedProfile, error) {
	if s.delegation == nil || s.validateLabOwner(ctx, r.Grant) != nil || s.delegation.Register(ctx, r) != nil {
		return nil, delegationUnavailable()
	}
	return s.delegation.Profiles(), nil
}

// ReadDelegation is the sole service caller. Ownership/project facts come from
// AO; question/paths carry no provider, account, credential or source-root input.
func (s *Service) ReadDelegation(ctx context.Context, id domain.SessionID, r reporting.DelegationRequest) (reporting.DelegationReply, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if strings.TrimSpace(r.Question) == "" || len(r.Question) > 16<<10 || len(r.Paths) == 0 || len(r.Paths) > 16 || len(r.Focus) > 4096 || r.OutputLimit < 0 || r.OutputLimit > 65536 {
		return reporting.DelegationReply{}, apierr.Invalid("INVALID_DELEGATION_REQUEST", "A bounded question and explicit relative paths are required", nil)
	}
	for _, path := range r.Paths {
		if !filepath.IsLocal(path) || filepath.Clean(path) != path {
			return reporting.DelegationReply{}, apierr.Invalid("INVALID_DELEGATION_PATH", "Paths must be explicit relative files", nil)
		}
	}
	rec, exists, err := s.store.GetSession(ctx, id)
	if err != nil {
		return reporting.DelegationReply{}, err
	}
	if !exists {
		return reporting.DelegationReply{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if s.delegation == nil {
		return reporting.DelegationReply{}, delegationUnavailable()
	}
	revision := s.delegation.PolicyRevision(s.identity(rec))
	i, p, err := s.delegationFacts(ctx, id)
	if err != nil {
		return reporting.DelegationReply{}, err
	}
	if rec.IsTerminated {
		return reporting.DelegationReply{Schema: "hcg.delegate-read/1", Status: "unavailable", Reason: "session_unavailable", SessionID: string(id), ProjectID: string(rec.ProjectID)}, nil
	}
	out := s.delegation.Read(ctx, i, p, r, revision)
	if out.Status == "ok" || out.Status == "not_found" {
		current, ok, err := s.store.GetSession(ctx, id)
		if err != nil || !ok || current.IsTerminated || current.ControllerOwner() != rec.ControllerOwner() || current.ProjectID != rec.ProjectID || current.Metadata.WorkspacePath != rec.Metadata.WorkspacePath {
			out.Status, out.Reason, out.Answer = "unavailable", "policy_or_identity_changed", ""
			out.Findings = []reporting.DelegationFinding{}
			out.Evidence = []reporting.DelegationEvidence{}
		}
	}
	return out, nil
}

// DelegationMountRequested resolves fixed metadata before native launch.
func (s *Service) DelegationMountRequested(n ports.HCGDelegationMount) bool {
	if s.delegation == nil {
		return false
	}
	return s.delegation.MountRequested(reporting.Identity{DataRoot: n.DataRoot, ProjectID: string(n.ProjectID), SessionID: string(n.SessionID), Role: string(n.Kind), Mode: string(n.Mode), Harness: string(n.Harness)})
}
