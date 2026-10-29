package store

import (
	"bytes"
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// EnsureAgentSettingsSigningKey is daemon-private control state. It has no
// response/audit/CDC projection; concurrent startups cannot replace a key.
func (s *Store) EnsureAgentSettingsSigningKey(ctx context.Context, candidate []byte) ([]byte, error) {
	if len(candidate) != 32 {
		return nil, ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer s.writeMu.Unlock()
	var result []byte
	err := s.inTx(ctx, "initialize settings signing authority", func(q *gen.Queries) error {
		if err := q.InsertAgentSettingsSigningKey(ctx, candidate); err != nil {
			return err
		}
		key, err := q.GetAgentSettingsSigningKey(ctx)
		if err != nil {
			return err
		}
		if len(key) != 32 {
			return ports.ErrAgentSettingsGrantInvalid
		}
		result = bytes.Clone(key)
		return nil
	})
	return result, err
}
