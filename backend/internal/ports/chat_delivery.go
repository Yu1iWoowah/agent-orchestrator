package ports

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrChatDeliveryUnverified retains the original execution instead of replaying it.
var ErrChatDeliveryUnverified = errors.New("chat delivery evidence is unavailable")

// ErrChatDeliveryDenied is a host reply without error text that confirms no
// record. It is still unverified, never proof of non-delivery.
var ErrChatDeliveryDenied = fmt.Errorf("%w: host answered without a record", ErrChatDeliveryUnverified)

// ErrChatShutdownNotSettled refuses a normal detach without settlement evidence.
var ErrChatShutdownNotSettled = errors.New("shutdown_not_settled")

// ChatShutdownSideError carries safe diagnostics without provider text or paths.
type ChatShutdownSideError struct {
	SideID string `json:"sideId"`
	Reason string `json:"reason"`
	Orphan bool   `json:"orphan"`
}

func (e *ChatShutdownSideError) Error() string {
	return "shutdown_not_settled: protected side " + e.SideID + ": " + e.Reason + "; use protected_side_chats"
}

func (*ChatShutdownSideError) Unwrap() error { return ErrChatShutdownNotSettled }

// ChatShutdownBlockedError names what kept shutdown from settling: the session
// (empty for a service-wide condition) and a fixed reason code. It carries no
// provider text; the cause stays available to internal classification through Unwrap.
type ChatShutdownBlockedError struct {
	SessionID string
	Reason    string
	Cause     error
}

func (e *ChatShutdownBlockedError) Error() string {
	text := "shutdown_not_settled: " + e.Reason
	if e.SessionID != "" {
		text += " (session " + e.SessionID + ")"
	}
	return text
}

// ChatDeliveryJournalError identifies a journal failure at its source without
// formatting the original storage or decoder error.
type ChatDeliveryJournalError struct {
	Stage string
	Cause error
}

func (e *ChatDeliveryJournalError) Error() string { return "chat delivery journal: " + e.Stage }
func (e *ChatDeliveryJournalError) Unwrap() error { return e.Cause }

// ChatDeliveryDecodeError marks a decoder error whose text may contain journal values.
type ChatDeliveryDecodeError struct{ Cause error }

func (e *ChatDeliveryDecodeError) Error() string { return e.Cause.Error() }
func (e *ChatDeliveryDecodeError) Unwrap() error { return e.Cause }

func (e *ChatShutdownBlockedError) Unwrap() []error {
	return []error{ErrChatShutdownNotSettled, e.Cause}
}

// ChatDeliveryGapError is a host flush refusal that names the native methods
// still lacking settlement evidence. It carries method names only.
type ChatDeliveryGapError struct {
	Methods []string
}

func (e *ChatDeliveryGapError) Error() string {
	return "shutdown_not_settled: host journal gap: " + strings.Join(e.Methods, ",")
}

func (*ChatDeliveryGapError) Unwrap() error { return ErrChatShutdownNotSettled }

// ChatDeliveryEpochJudge proves that a recorded provider epoch belongs to an
// earlier boot, so neither its host nor any process it started can still run.
type ChatDeliveryEpochJudge interface {
	DeliveryEpochFromPreviousBoot(epoch string) bool
}

// ChatDeliveryEpochReporter names the provider host that currently holds
// outbound custody; empty when no host is attached.
type ChatDeliveryEpochReporter interface {
	OutboundProviderEpoch() string
}

// ChatDeliveryLookup contains correlation keys only, never a prompt or path.
type ChatDeliveryLookup struct {
	AttemptID         string
	PreparedTurnID    string
	ProviderRequestID string
	ProviderEpoch     string
}

// ChatDeliveryObserver queries the original execution and flushes its durable
// custody. Missing records or an active-prompt boolean are not negative proof.
// Flush may cancel an unforwarded request only with positive cancellation proof.
type ChatDeliveryObserver interface {
	QueryDeliveryAttempt(context.Context, ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error)
	FenceAndFlushOutbound(context.Context, []ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error)
}

// ChatDeliveryAcknowledger releases host evidence only after its durable receipt
// is committed. Older observers may keep bounded unacknowledged evidence.
type ChatDeliveryAcknowledger interface {
	AcknowledgeDeliveryAttempt(context.Context, ChatDeliveryLookup) error
}

// ChatOutboundReconciler obtains fresh native state for an uncovered operation
// from its original host. Nil requires positive idle/terminal evidence and a
// successful host flush, never an empty local in-flight flag or an RPC ACK.
// This does not change canonical delivery dispositions.
type ChatOutboundReconciler interface {
	OutboundProviderEpoch() string
	ReconcileOutbound(context.Context) error
}
