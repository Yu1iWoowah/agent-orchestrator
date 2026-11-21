package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SetRCAClaudeLaunchControl wires only the normal main Chat launch. Side and
// review paths do not receive an environment callback or capture namespace.
func (s *Service) SetRCAClaudeLaunchControl(control ports.RCAClaudeLaunchControl) {
	s.mu.Lock()
	s.rcaClaude = control
	s.mu.Unlock()
}
func (s *Service) invalidateClaudeRCA(id domain.SessionID) {
	s.mu.RLock()
	control := s.rcaClaude
	s.mu.RUnlock()
	if control != nil {
		func() { defer func() { _ = recover() }(); control.InvalidateClaudeLaunch(id) }()
	}
}
func (s *Service) claudeRCAForLaunch(cfg StartConfig, generation string) (ports.RCAClaudeEnvironment, func(context.Context, bool, bool)) {
	owner := conversationOwner(cfg)
	if cfg.ProjectID != "agent-orchestrator" || cfg.Harness != domain.HarnessClaudeCode || cfg.ReviewerSettings != nil || owner.Kind != domain.ConversationOwnerSession || owner.ID != string(cfg.SessionID) || (cfg.Kind != domain.KindOrchestrator && cfg.Kind != domain.KindWorker) {
		return nil, nil
	}
	s.mu.RLock()
	control := s.rcaClaude
	s.mu.RUnlock()
	if control == nil {
		return nil, nil
	}
	s.invalidateClaudeRCA(cfg.SessionID)
	notice := ports.RCAClaudeLaunch{SessionID: cfg.SessionID, ProjectID: cfg.ProjectID, Kind: cfg.Kind, DataRoot: cfg.DataDir, Generation: generation}
	environment := func(ctx context.Context, runtime ports.RCAClaudeRuntime, keys []string) (delta map[string]string) {
		defer func() {
			if recover() != nil {
				delta = nil
			}
		}()
		return control.PrepareClaudeEnvironment(ctx, notice, runtime, keys)
	}
	ready := func(ctx context.Context, committed, live bool) {
		defer func() { _ = recover() }()
		if !committed {
			control.InvalidateClaudeLaunch(cfg.SessionID)
			return
		}
		control.ClaudeLaunchReady(ctx, notice, live)
	}
	return environment, ready
}
