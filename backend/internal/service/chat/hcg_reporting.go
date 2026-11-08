package chat

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SetHCGReportingObserver wires the optional fixed metadata consumer.
func (s *Service) SetHCGReportingObserver(observer ports.HCGReportingLaunchObserver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hcgReporting = observer
}

func (s *Service) notifyHCGReporting(cfg StartConfig, live bool) {
	owner := conversationOwner(cfg)
	if owner.Kind != domain.ConversationOwnerSession || owner.ID != string(cfg.SessionID) || cfg.ReviewerSettings != nil || (cfg.Kind != domain.KindWorker && cfg.Kind != domain.KindOrchestrator) {
		return
	}
	s.mu.RLock()
	observer := s.hcgReporting
	s.mu.RUnlock()
	ports.NotifyHCGReportingLaunch(observer, ports.HCGReportingLaunch{Origin: ports.HCGReportingMainChat, Owner: owner, SessionID: cfg.SessionID, ProjectID: cfg.ProjectID, Kind: cfg.Kind, Harness: cfg.Harness, Mode: domain.SessionModeChat, DataRoot: cfg.DataDir, WorkspacePath: cfg.WorkspacePath, ArtifactDir: cfg.ArtifactDir, CommittedOwner: cfg.ExpectedControllerOwner, Fresh: !live})
}
