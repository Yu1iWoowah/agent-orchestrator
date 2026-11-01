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
	GuardianContinuityRequests(context.Context, domain.GuardianCursor, int64, []string) ([]domain.GuardianRequest, error)
	GuardianHasRequests(context.Context, int64, string) (bool, error)
	GuardianCursors(context.Context, int64) ([]domain.GuardianCursor, error)
	ApplyGuardianBatch(context.Context, domain.GuardianBatch) error
	ObserveGuardianAnomalies(context.Context, domain.GuardianOwner, domain.GuardianSourceState, map[domain.GuardianCode]int64, time.Time) error
	RecordGuardianAnomaly(context.Context, domain.SessionID, domain.GuardianCode, time.Time) error
	CleanupGuardian(context.Context, time.Time) error
}
