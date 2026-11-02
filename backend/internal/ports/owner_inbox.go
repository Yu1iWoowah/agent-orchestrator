package ports

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// OwnerInbox is the H-owned intake boundary. Production cutover also requires
// S-G0's outbound permit/lane and native defer exit; this interface grants no I/O.
type OwnerInbox interface {
	AcceptReport(context.Context, domain.InboxReportIntent) (domain.InboxReceipt, error)
	ClaimBatch(context.Context, domain.InboxBatchPlan) (domain.InboxReceipt, error)
	BindAcceptAndAckAO(context.Context, domain.InboxInputPlan) (domain.InboxReceipt, error)
	GetByActionKey(context.Context, domain.ProjectID, string) (domain.InboxReceipt, bool, error)
}

// OwnerInboxIntakeTransaction is instantiated by the storage adapter against
// H's current transaction. It may execute only S-owned intake eligibility,
// binding and hold SQL. No public store reentry, commit, locks or I/O is allowed.
// The real implementation belongs to S-G0, not H or an allow fallback.
type OwnerInboxIntakeTransaction interface {
	EvaluateAndBind(context.Context, domain.InboxInputPlan) (domain.InboxIntakeReceipt, error)
}

// OwnerInboxMetadataTransaction may enlist only U1-owned metadata in the same
// transaction. Stage/source-version identify the effect across direct/piggyback,
// claim retries and owner changes. An enabled adapter's error is fatal.
type OwnerInboxMetadataTransaction interface {
	Enlist(context.Context, domain.InboxMetadataPlan) ([]string, error)
}
