// Package processscope captures the original Linux execution scope.
package processscope

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// Scope retains the original process identities needed for later inspection.
type Scope = domain.CompactionProcessScope
