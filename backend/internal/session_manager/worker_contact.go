package sessionmanager

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// A seed exists before this final prompt read. Activation either captured that
// worker as a notification target, or completed before its fresh contact read.
// This also avoids caching an old contact across slow workspace preparation.
func (m *Manager) refreshWorkerSystemPrompt(ctx context.Context, rec domain.SessionRecord, chat bool, previous, file string) (string, string, error) {
	if rec.Kind != domain.KindWorker {
		return previous, file, nil
	}
	current, err := m.buildSystemPrompt(ctx, rec.Kind, rec.ProjectID, rec.ID, rec.RequestedOutput, chat)
	if err != nil {
		return "", "", err
	}
	if current == previous {
		return current, file, nil
	}
	file, err = m.prepareSystemPromptFile(rec.ID, rec.Harness, current)
	return current, file, err
}
