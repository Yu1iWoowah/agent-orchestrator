package lifecycle

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// FinalizeReplacementUsage collects final usage without terminating the source
// or reaping its containers. The replacement coordinator owns the separately
// verified, atomic retirement commit; generic MarkTerminated is destructive.
func (m *Manager) FinalizeReplacementUsage(ctx context.Context, expected domain.SessionRecord) error {
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, found, err := m.store.GetSession(ctx, expected.ID)
		if err != nil {
			return err
		}
		if !found || expected.IsTerminated || current.IsTerminated || expected.Kind != domain.KindOrchestrator || current.Kind != domain.KindOrchestrator || current.ProjectID != expected.ProjectID || current.Revision != expected.Revision || current.ControllerOwner() != expected.ControllerOwner() || current.Metadata.RuntimeHandleID != expected.Metadata.RuntimeHandleID || current.Metadata.BrowserCapabilityVerifier != expected.Metadata.BrowserCapabilityVerifier {
			return domain.ErrReplacementSourceChanged
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	m.mu.Lock()
	finalizer := m.usageFinalizer
	m.mu.Unlock()
	finalizeSessionUsage(ctx, expected.ID, expected.Metadata.RuntimeLaunchID, expected.Revision, finalizer)
	return check()
}
