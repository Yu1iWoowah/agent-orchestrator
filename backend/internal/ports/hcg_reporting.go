package ports

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// HCGReportingOrigin is positive evidence of the producer's main-session path.
type HCGReportingOrigin string

// Fixed origins distinguish successful main launches from excluded consumers.
const (
	HCGReportingUnknown  HCGReportingOrigin = ""
	HCGReportingMainChat HCGReportingOrigin = "main_chat"
	HCGReportingMainTUI  HCGReportingOrigin = "main_tui"
	HCGReportingReview   HCGReportingOrigin = "review"
	HCGReportingSide     HCGReportingOrigin = "side"
)

// HCGReportingLaunch contains committed metadata only, never env or execution.
type HCGReportingLaunch struct {
	Origin                               HCGReportingOrigin
	Owner                                domain.ConversationOwner
	SessionID                            domain.SessionID
	ProjectID                            domain.ProjectID
	Kind                                 domain.SessionKind
	Harness                              domain.AgentHarness
	Mode                                 domain.SessionMode
	DataRoot, WorkspacePath, ArtifactDir string
	CommittedOwner                       domain.SessionControllerOwner
	Fresh                                bool
}

// HCGReportingLaunchObserver captures metadata without waiting for report I/O.
type HCGReportingLaunchObserver interface{ ReportingLaunchReady(HCGReportingLaunch) }

// NotifyHCGReportingLaunch cannot turn an already successful native launch into
// a failure, even if the optional metadata consumer panics.
func NotifyHCGReportingLaunch(observer HCGReportingLaunchObserver, notice HCGReportingLaunch) {
	if observer == nil {
		return
	}
	defer func() { _ = recover() }()
	observer.ReportingLaunchReady(notice)
}
