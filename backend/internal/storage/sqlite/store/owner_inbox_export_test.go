package store

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// OwnerInboxTestParticipants is compiled only for storage contract fixtures.
// Production has no registration or allow adapter for the absent S-G0 gate.
func OwnerInboxTestParticipants(c *OwnerInboxCore, metadataEnabled bool, factory func(gen.DBTX) (ports.OwnerInboxIntakeTransaction, ports.OwnerInboxMetadataTransaction)) {
	c.participants = factory
	c.metadataEnabled = metadataEnabled
}
