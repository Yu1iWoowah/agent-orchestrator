package hcg

import (
	"context"
	"os"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

func CheckDelegationOwnerStartup(ctx context.Context, root string) error {
	return reporting.CheckDelegationOwnerStartup(ctx, root)
}

// validateLabOwner checks the actual open SQLite store, not a caller's claim
// that another directory or a newly created empty DB contains the owner.
func (s *Service) validateLabOwner(ctx context.Context, grant *reporting.DelegationLabGrant) error {
	if grant == nil || grant.OwnerRoot != s.root || !filepath.IsAbs(s.root) || filepath.Clean(s.root) != s.root {
		return delegationUnavailable()
	}
	canonical, ok := s.store.(interface {
		CanonicalDataRoot(context.Context) (string, error)
	})
	if !ok {
		return delegationUnavailable()
	}
	root, err := canonical.CanonicalDataRoot(ctx)
	if err != nil || root != s.root {
		return delegationUnavailable()
	}
	resolved, err := filepath.EvalSymlinks(root)
	info, statErr := os.Lstat(root)
	if err != nil || resolved != root || statErr != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return delegationUnavailable()
	}
	owner := false
	for _, binding := range grant.Bindings {
		rec, found, err := s.store.GetSession(ctx, domain.SessionID(binding.SessionID))
		if err != nil || !found || string(rec.ProjectID) != binding.ProjectID || (rec.Kind != domain.KindWorker && rec.Kind != domain.KindOrchestrator) {
			return delegationUnavailable()
		}
		if _, found, err := s.store.GetProject(ctx, binding.ProjectID); err != nil || !found {
			return delegationUnavailable()
		}
		owner = owner || binding == grant.CanonicalOwner
	}
	if !owner {
		return delegationUnavailable()
	}
	return nil
}
