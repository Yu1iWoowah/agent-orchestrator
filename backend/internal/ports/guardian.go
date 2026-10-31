package ports

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// GuardianStore is separate from the accounting and approval ports. Its
// transaction owns only the metadata whitelist and independent source cursor.
type GuardianStore interface {
	GuardianOwners(context.Context, time.Time) ([]domain.GuardianOwner, error)
	GuardianCursor(context.Context, int64, string) (domain.GuardianCursor, error)
	GuardianPosition(context.Context, int64, string) (domain.GuardianCursor, error)
	GuardianCheckpoint(context.Context, domain.GuardianCursor) (domain.GuardianRequest, bool, error)
	GuardianCursors(context.Context, int64) ([]domain.GuardianCursor, error)
	ApplyGuardianBatch(context.Context, domain.GuardianBatch) error
	RecordGuardianAnomaly(context.Context, domain.SessionID, domain.GuardianCode, time.Time) error
	CleanupGuardian(context.Context, time.Time) error
}
