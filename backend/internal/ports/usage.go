package ports

import (
	"context"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ExecutionUsageRecorder accepts only receipts from the trusted HCG executor.
// No CLI/HTTP token-import endpoint implements this port.
type ExecutionUsageRecorder interface {
	RecordExecutionUsage(context.Context, domain.ExecutionUsageReceipt) error
}
