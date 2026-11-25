// Package hcg connects AO-owned session facts to the fixed delegation
// consumer.
package hcg

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

// Store exposes only AO identity and policy reads to delegation.
type Store interface {
	ListProjects(context.Context) ([]domain.ProjectRecord, error)
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
}

// Service keeps delegation separate from native execution and canonical writes.
type Service struct {
	delegation *reporting.DelegationConsumer
	store      Store
	root       string
	sharedRoot func(context.Context, domain.ProjectID) (string, error)
}

// New builds the delegation service without a provider override.
func New(ctx context.Context, root string, store Store) *Service {
	return NewWithDelegationProvider(ctx, root, store, nil, nil, nil)
}

// NewWithDelegationProvider is a trusted constructor seam for the locked owner
// and offline fixtures.
func NewWithDelegationProvider(ctx context.Context, root string, store Store, provider reporting.DelegationProvider, checkpoint func(context.Context, string), logger *slog.Logger) *Service {
	s := &Service{store: store, root: root}
	if writer, ok := store.(interface {
		InsertExecutionUsage(context.Context, domain.ExecutionUsageReceipt, string) error
	}); ok {
		s.delegation = reporting.NewDelegation(root, delegationReceiptRecorder{canonical: usagesvc.NewExecutionRecorder(writer)}, provider, checkpoint, logger)
		recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 5*time.Second)
		// Failed projections retain their pending receipt and worst hold. Startup
		// never retries the provider or changes native session/policy state.
		_ = s.delegation.RecoverDelegationReceipts(recoveryCtx)
		recoveryCancel()
		if projects, err := store.ListProjects(ctx); err == nil {
			for _, p := range projects {
				s.delegation.ProjectPolicyChanged(p.ID, delegationPolicy(p.Config.DelegationPolicySnapshot()))
			}
		}
	}
	return s
}

// Close stops in-flight delegated reads, never a native process.
func (s *Service) Close() {
	if s.delegation != nil {
		s.delegation.Close()
	}
}

// SetSharedRoot lets orchestrators also read the project's shared directory.
func (s *Service) SetSharedRoot(resolve func(context.Context, domain.ProjectID) (string, error)) {
	s.sharedRoot = resolve
}

// identity gives orchestrators two extra read-only roots: all session
// artifacts under the data root and the project's shared directory.
func (s *Service) identity(ctx context.Context, rec domain.SessionRecord) reporting.Identity {
	idn := reporting.Identity{DataRoot: s.root, ProjectID: string(rec.ProjectID), SessionID: string(rec.ID), Role: string(rec.Kind), Mode: string(domain.NormalizeSessionMode(rec.Mode)), Harness: string(rec.Harness), Worktree: rec.Metadata.WorkspacePath}
	if rec.Kind == domain.KindOrchestrator {
		idn.ReadRoots = append(idn.ReadRoots, filepath.Join(s.root, "artifacts"))
		if s.sharedRoot != nil {
			if shared, err := s.sharedRoot(ctx, rec.ProjectID); err == nil {
				idn.ReadRoots = append(idn.ReadRoots, shared)
			}
		}
	}
	return idn
}
