package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSettingsHandoffConflictIsPublic409(t *testing.T) {
	err := mapSessionError(fmt.Errorf("another settings handoff: %w", ports.ErrAgentSettingsConflict))
	var got *apierr.Error
	if !errors.As(err, &got) || got.Kind != apierr.KindConflict || got.Code != "AGENT_SETTINGS_CONFLICT" {
		t.Fatalf("settings/account/interface contention leaked as generic failure: %T %v", err, err)
	}
}
