package daemon

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.ReplacementChatRuntime = chatLauncher{}

func (c chatLauncher) LookupReplacementChatResume(ctx context.Context, id domain.SessionID, generation, native string) (ports.ReplacementResumeObservation, error) {
	if err := c.waitForPersistentHostReconcile(ctx); err != nil {
		return ports.ReplacementResumeObservation{}, err
	}
	return c.svc.LookupReplacementChatResume(ctx, id, generation, native)
}

func (c chatLauncher) PreflightReplacementChat(ctx context.Context, harness domain.AgentHarness, permissions ports.PermissionMode) error {
	return c.svc.PreflightReplacementChat(ctx, harness, permissions)
}
func (c chatLauncher) ObserveReplacementChatSource(ctx context.Context, op domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	if err := c.waitForPersistentHostReconcile(ctx); err != nil {
		return ports.ReplacementSourceObservation{}, err
	}
	return c.svc.ObserveReplacementChatSource(ctx, op)
}
func (c chatLauncher) EnsureReplacementChatPreparation(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	if err := c.waitForPersistentHostReconcile(ctx); err != nil {
		return ports.ReplacementPreparationDelivery{}, err
	}
	return c.svc.EnsureReplacementChatPreparation(ctx, op, prompt, allowNew)
}
func (c chatLauncher) QuiesceReplacementChatSource(ctx context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	if err := c.waitForPersistentHostReconcile(ctx); err != nil {
		return domain.ReplacementControllerStop{}, err
	}
	return c.svc.QuiesceReplacementChatSource(ctx, op)
}
func (c chatLauncher) ObserveReplacementChatSuccessor(ctx context.Context, op domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	if err := c.waitForPersistentHostReconcile(ctx); err != nil {
		return ports.ReplacementSourceObservation{}, err
	}
	return c.svc.ObserveReplacementChatSuccessor(ctx, op)
}
func (c chatLauncher) EnsureReplacementChatBootstrap(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	if err := c.waitForPersistentHostReconcile(ctx); err != nil {
		return ports.ReplacementPreparationDelivery{}, err
	}
	return c.svc.EnsureReplacementChatBootstrap(ctx, op, prompt, allowNew)
}
func (c chatLauncher) QuiesceReplacementChatSuccessor(ctx context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	if err := c.waitForPersistentHostReconcile(ctx); err != nil {
		return domain.ReplacementControllerStop{}, err
	}
	return c.svc.QuiesceReplacementChatSuccessor(ctx, op)
}
func (c chatLauncher) EnsureReplacementChatProbe(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	if err := c.waitForPersistentHostReconcile(ctx); err != nil {
		return ports.ReplacementPreparationDelivery{}, err
	}
	return c.svc.EnsureReplacementChatProbe(ctx, op, prompt, allowNew)
}
func (c chatLauncher) QueueReplacementIntake(ctx context.Context, id domain.SessionID, msg ports.ChatUserMessage) (domain.ConversationTurn, error) {
	return c.svc.QueueReplacementIntake(ctx, id, msg)
}
