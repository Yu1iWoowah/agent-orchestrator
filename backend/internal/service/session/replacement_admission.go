package session

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type orchestratorProjectGate interface {
	WithOrchestratorProjectGate(context.Context, domain.ProjectID, func(context.Context) error) error
}
type replacementAdmissionReader interface {
	GetActiveOrchestratorReplacement(context.Context, domain.ProjectID) (domain.OrchestratorReplacement, bool, error)
}

// WithOrchestratorProjectGate shares production Manager admission with legacy
// clean/ensure and durable replacement operations. Focused fakes use the local gate.
func (s *Service) WithOrchestratorProjectGate(ctx context.Context, project domain.ProjectID, fn func(context.Context) error) error {
	if manager, ok := s.manager.(orchestratorProjectGate); ok {
		return manager.WithOrchestratorProjectGate(ctx, project, fn)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	release := s.lockOrchestratorProject(project)
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}
