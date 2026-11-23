// Package hcg connects AO-owned session facts to the fixed delegation
// consumer.
package hcg

import (
	"context"
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
	metadata   *metadataObserver
}

// New builds the delegation service without a provider override.
func New(ctx context.Context, root string, store Store) *Service {
	return NewWithDelegationProvider(ctx, root, store, nil, nil)
}

// NewWithDelegationProvider is a trusted constructor seam for the locked lab
// owner and offline fixtures. Registration still requires an explicit run grant.
func NewWithDelegationProvider(ctx context.Context, root string, store Store, provider reporting.DelegationProvider, checkpoint func(context.Context, string)) *Service {
	s := &Service{store: store, root: root}
	if writer, ok := store.(interface {
		InsertExecutionUsage(context.Context, domain.ExecutionUsageReceipt, string) error
	}); ok {
		s.delegation = reporting.NewDelegation(root, delegationReceiptRecorder{canonical: usagesvc.NewExecutionRecorder(writer)}, provider, checkpoint)
		recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 5*time.Second)
		// Failed projections retain their pending receipt and worst hold. Startup
		// never retries the provider or changes native session/policy state.
		if grant := s.delegation.LabGrant(); grant == nil || s.validateLabOwner(recoveryCtx, grant) == nil {
			_ = s.delegation.RecoverDelegationReceipts(recoveryCtx)
		}
		recoveryCancel()
	}
	s.startMetadata(ctx)
	return s
}

func (s *Service) identity(rec domain.SessionRecord) reporting.Identity {
	idn := reporting.Identity{DataRoot: s.root, ProjectID: string(rec.ProjectID), SessionID: string(rec.ID), Role: string(rec.Kind), Mode: string(domain.NormalizeSessionMode(rec.Mode)), Harness: string(rec.Harness), Worktree: rec.Metadata.WorkspacePath, ArtifactDir: rec.Metadata.ArtifactDir, ControllerGeneration: rec.Metadata.ControllerGeneration}
	if rec.Mode == domain.SessionModeChat {
		idn.LaunchID, idn.NativeID = rec.Metadata.AgentSettingsLaunchID, rec.Metadata.ProviderConversationID
	} else {
		idn.LaunchID = rec.Metadata.RuntimeLaunchID
		if rec.Metadata.AgentSessionIDLaunchID == idn.LaunchID && idn.LaunchID != "" {
			idn.NativeID = rec.Metadata.AgentSessionID
		}
	}
	return idn
}
