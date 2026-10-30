package agentsettings

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type tuiWriterSettler interface {
	SettleTUIAgentSettings(context.Context, domain.SessionID, string, int64, bool) (domain.AgentSettingsOperation, error)
}

// SettleWriter is a manual cleanup boundary, not a settings grant capability.
// Local callers retain the same unverified-operator scope as Kill. The runtime
// coordinator checks termination, version and positive physical stop evidence.
func (c *Control) SettleWriter(ctx context.Context, caller Caller, id domain.SessionID, operationID string, version int64, stopIfAlive bool) (domain.AgentSettingsOperation, error) {
	if caller != (Caller{}) {
		return domain.AgentSettingsOperation{}, apierr.Forbidden("AGENT_SETTINGS_OPERATOR_REQUIRED", "Native writer settlement requires an explicit local operator action")
	}
	if id == "" || operationID == "" || version < 0 {
		return domain.AgentSettingsOperation{}, settingsError(ports.ErrAgentSettingsInvalid)
	}
	if err := ctx.Err(); err != nil {
		return domain.AgentSettingsOperation{}, settingsError(err)
	}
	runtime, ok := c.tui.(tuiWriterSettler)
	if !ok {
		return domain.AgentSettingsOperation{}, apierr.Unavailable("AGENT_SETTINGS_UNAVAILABLE", "Native writer settlement is unavailable")
	}
	op, err := runtime.SettleTUIAgentSettings(ctx, id, operationID, version, stopIfAlive)
	if err != nil {
		return domain.AgentSettingsOperation{}, settingsError(err)
	}
	if op.TUI == nil || !op.TUI.TerminationVerified || op.TUI.UnconfirmedRuntime {
		return domain.AgentSettingsOperation{}, apierr.Conflict("AGENT_SETTINGS_WRITER_UNCONFIRMED", "Native writer stop evidence is unavailable; the launch veto is retained", nil)
	}
	return op, nil
}
