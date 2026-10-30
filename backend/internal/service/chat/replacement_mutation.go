package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ReplacementMutationGate orders public provider/history changes before
// replacement acceptance. Ordinary Send remains a durable queued intake path.
type ReplacementMutationGate interface {
	BeginReplacementMutation(context.Context, domain.SessionID) (context.Context, func(), error)
}

// SetReplacementMutationGate is late-bound before the daemon accepts requests.
func (s *Service) SetReplacementMutationGate(gate ReplacementMutationGate) {
	s.mu.Lock()
	s.replacementMutationGate = gate
	s.mu.Unlock()
}

func (s *Service) beginReplacementMutation(ctx context.Context, id domain.SessionID) (context.Context, func(), error) {
	s.mu.RLock()
	gate := s.replacementMutationGate
	s.mu.RUnlock()
	if gate != nil {
		return gate.BeginReplacementMutation(ctx, id)
	}
	// Non-daemon service embeddings still honor durable admission.
	return ctx, func() {}, s.replacementWakeFence(ctx, id)
}

// replacementWakeFence keeps view leases and ordinary queued sends from waking
// a controller parked for an active replacement. Controlled native resume uses
// the Manager's operation-bound path, never this automatic wake callback.
func (s *Service) replacementWakeFence(ctx context.Context, id domain.SessionID) error {
	reader, ok := s.sessions.(interface {
		ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error)
	})
	if !ok {
		return nil
	}
	fenced, err := reader.ReplacementWakeFenced(ctx, id)
	if err != nil {
		return err
	}
	if fenced {
		return domain.ErrReplacementIntakeFenced
	}
	return nil
}
