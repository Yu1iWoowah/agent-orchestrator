package agentsettings

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// HistoryEntry carries original attribution, never a client-claimed actor.
// Native runtime handles and the provider plan remain private on the operation.
type HistoryEntry struct {
	domain.AgentSettingsOperation
	WriterStatus string `json:"writerStatus,omitempty" enum:"unconfirmed,stopped"`
}

// WriterRecovery identifies an abandoned obligation, not permission to launch.
type WriterRecovery struct {
	OperationID string `json:"operationId"`
	Version     int64  `json:"version"`
	State       string `json:"state" enum:"writer_unconfirmed"`
}

type settingsHistoryReader interface {
	ListAgentSettingsOperations(context.Context, domain.SessionID, int) ([]domain.AgentSettingsOperation, error)
}

func (s *Service) readHistory(ctx context.Context, snapshot *Snapshot) error {
	reader, ok := s.store.(settingsHistoryReader)
	if !ok {
		return nil
	}
	operations, err := reader.ListAgentSettingsOperations(ctx, snapshot.SessionID, 50)
	if err != nil {
		return err
	}
	snapshot.History = make([]HistoryEntry, 0, len(operations))
	for _, op := range operations {
		entry := HistoryEntry{AgentSettingsOperation: op}
		if op.TUI != nil && op.Phase == domain.AgentSettingsCancelled && op.ErrorCode == "AGENT_SETTINGS_SESSION_TERMINATED" {
			if op.TUI.TerminationVerified && !op.TUI.UnconfirmedRuntime {
				entry.WriterStatus = "stopped"
			} else {
				entry.WriterStatus = "unconfirmed"
				if snapshot.IsTerminated && snapshot.WriterRecovery == nil {
					snapshot.WriterRecovery = &WriterRecovery{OperationID: op.ID, Version: op.Version, State: "writer_unconfirmed"}
				}
			}
		}
		snapshot.History = append(snapshot.History, entry)
	}
	return nil
}
