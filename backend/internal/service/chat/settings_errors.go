package chat

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Native diagnostics can echo any launch credential, including a grant issued
// by a previous daemon. Retain only known classifications, never the original
// error/cause; it must not reach access logs, capture, or durable turn errors.
func providerSettingsError(err error) error {
	if err == nil {
		return nil
	}
	for _, classification := range []error{ports.ErrAgentSettingsConflict, ports.ErrAgentSettingsUnsupported, ports.ErrAgentSettingsInvalid, ports.ErrAgentSettingsCapabilitiesUnavailable, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, classification) {
			return classification
		}
	}
	return apierr.New(apierr.KindInternal, "AGENT_SETTINGS_PROVIDER_FAILED", "The provider could not apply or restore settings", nil)
}
