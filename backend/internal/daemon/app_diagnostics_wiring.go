package daemon

import (
	"time"

	appruntime "github.com/aoagents/agent-orchestrator/backend/internal/appdiagnostics"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	appdiag "github.com/aoagents/agent-orchestrator/backend/internal/service/appdiagnostics"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
)

// Storage cleanup is unconditional; only the trusted desktop channel enables
// the capability-checked API. Run calls this before publishing its listeners.
func newAppDiagnosticsServices(dataDir string, sessions *sessionsvc.Service, broker *appruntime.Broker, enabled bool) (*appdiag.Service, controllers.AppDiagnosticsService, error) {
	service, err := appdiag.New(dataDir, sessions, broker, time.Now)
	if err != nil {
		return nil, nil, err
	}
	if !enabled {
		return service, nil, nil
	}
	broker.BindAuthority(service)
	return service, service, nil
}
