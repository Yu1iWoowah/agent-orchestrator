package sessionmanager

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (m *Manager) notifyHCGReporting(rec domain.SessionRecord, configured bool) {
	if domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeTUI || (rec.Kind != domain.KindWorker && rec.Kind != domain.KindOrchestrator) {
		return
	}
	ports.NotifyHCGReportingLaunch(m.hcgReporting, ports.HCGReportingLaunch{DelegationConfigured: configured, Origin: ports.HCGReportingMainTUI, Owner: domain.SessionConversationOwner(rec.ID), SessionID: rec.ID, ProjectID: rec.ProjectID, Kind: rec.Kind, Harness: rec.Harness, Mode: domain.SessionModeTUI, DataRoot: m.dataDir, WorkspacePath: rec.Metadata.WorkspacePath, ArtifactDir: rec.Metadata.ArtifactDir, CommittedOwner: rec.ControllerOwner(), Fresh: true})
}
