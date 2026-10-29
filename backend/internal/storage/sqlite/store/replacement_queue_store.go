package store

import (
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func replacementQueueError(err error) error {
	if err != nil && strings.Contains(err.Error(), "ORCHESTRATOR_REPLACEMENT_INTAKE_FENCED") {
		return domain.ErrReplacementIntakeFenced
	}
	return err
}
