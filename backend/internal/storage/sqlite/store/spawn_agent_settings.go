package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// SaveSpawnAgentSettings pins a new session's decision before launch. The one
// UPDATE includes preferences and evidence; existing projection/CDC triggers
// participate in the same SQLite statement and roll back together on failure.
func (s *Store) SaveSpawnAgentSettings(ctx context.Context, id domain.SessionID, resolution domain.SpawnAgentSettingsResolution) error {
	if err := resolution.Settings.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(resolution)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.SaveSpawnAgentSettings(ctx, gen.SaveSpawnAgentSettingsParams{ID: id, Model: string(resolution.Settings.Model), Effort: string(resolution.Settings.Effort), FastMode: string(resolution.Settings.FastMode), Resolution: string(raw), AccountID: resolution.AccountID})
	if err != nil {
		return fmt.Errorf("save spawn agent settings: %w", err)
	}
	if rows == 1 {
		return nil
	}
	row, err := s.qw.GetSession(ctx, id)
	if err != nil {
		return settingsStoreError(err)
	}
	owner, err := s.qw.GetSessionAgentSettings(ctx, id)
	if err != nil {
		return settingsStoreError(err)
	}
	if row.SpawnAgentSettingsJson == string(raw) && owner.AccountID == resolution.AccountID {
		return nil
	}
	return ports.ErrAgentSettingsConflict
}

func decodeSpawnAgentSettings(raw string) *domain.SpawnAgentSettingsResolution {
	if raw == "" {
		return nil
	}
	var resolution domain.SpawnAgentSettingsResolution
	if json.Unmarshal([]byte(raw), &resolution) != nil {
		return nil
	}
	return &resolution
}

// The old runtime projections used a zero value for the native baseline.
// Keep that equivalent representation for legacy readers; the immutable spawn
// decision still distinguishes an explicit default from an omitted request.
func storedFastPreference(mode string) domain.AgentFastMode {
	if mode == string(domain.AgentFastDefault) {
		return ""
	}
	return domain.AgentFastMode(mode)
}
