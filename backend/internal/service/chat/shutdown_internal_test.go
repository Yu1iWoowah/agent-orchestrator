package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type shutdownGapConversation struct {
	ports.ChatConversation
	closed bool
}

func (c *shutdownGapConversation) Close() error { c.closed = true; return nil }

func TestStopAllRefusesUnmanagedSideBeforeClosingAnyTransport(t *testing.T) {
	for _, kind := range []string{"turn", "compact", "completed_without_delivery_receipt", "restore", "opening"} {
		t.Run(kind, func(t *testing.T) {
			conv := &shutdownUnverifiedConversation{shutdownGapConversation: &shutdownGapConversation{}}
			runtime := &sideRuntime{conv: conv}
			s := &Service{}
			s.sides = &sideManager{service: s, runtimes: map[string]*sideRuntime{"side": runtime}}
			switch kind {
			case "turn":
				runtime.activeTurnID = "original-side-turn"
			case "compact":
				runtime.compacting = true
			case "completed_without_delivery_receipt":
				runtime.outboundUnverified = true
			case "restore":
				s.sides.restoring = map[string]context.CancelFunc{"side": func() { t.Error("restore cancelled before preflight") }}
			case "opening":
				s.sides.opening = 1
			}
			if err := s.StopAll(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
				t.Fatalf("coverage gap returned normal success: %v", err)
			}
			if conv.closed {
				t.Fatal("side transport closed before settlement")
			}
			if s.shutdownQuiescing || s.sides.quiescing {
				t.Fatal("failed preflight prevented read/control recovery")
			}
		})
	}
}

func TestRPCDeadlineUsesProtocolClockAndInjectedCancellation(t *testing.T) {
	timer := make(chan time.Time, 1)
	c := &Controller{now: func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) }, after: func(delay time.Duration) <-chan time.Time {
		if delay != 10*time.Second {
			t.Fatalf("deadline budget=%s", delay)
		}
		return timer
	}}
	before := time.Now()
	ctx, cancel := c.deadlineContext(t.Context(), 10*time.Second)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(9*time.Second)) || deadline.After(time.Now().Add(11*time.Second)) {
		t.Fatalf("business date leaked to protocol deadline: %s", deadline)
	}
	timer <- time.Time{}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			t.Fatal(context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("injected timeout did not cancel RPC")
	}
}

type shutdownUnverifiedConversation struct{ *shutdownGapConversation }

func (c *shutdownUnverifiedConversation) QueryDeliveryAttempt(context.Context, ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	return domain.ChatDeliveryReceipt{}, ports.ErrChatDeliveryUnverified
}
func (c *shutdownUnverifiedConversation) FenceAndFlushOutbound(context.Context, []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	return nil, ports.ErrChatDeliveryUnverified
}
func TestIdleUnmanagedSideKeepsUnknownShutdownCoverage(t *testing.T) {
	runtime := &sideRuntime{conv: &shutdownGapConversation{}, outboundUnverified: true}
	s := &Service{}
	s.sides = &sideManager{service: s, runtimes: map[string]*sideRuntime{"side": runtime}}
	if err := s.PreflightShutdown(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("unknown side lost coverage: %v", err)
	}
	if !runtime.outboundUnverified {
		t.Fatal("idle is not settlement proof")
	}
}

type reviewDeliveryStore struct {
	Store
	chatDeliveryJournal
}

func (s *reviewDeliveryStore) PendingChatDeliveries(context.Context, domain.SessionID) ([]domain.ChatDeliveryAttempt, error) {
	return nil, errors.New("reviewer must not read worker delivery records")
}

type reviewFlushConversation struct {
	ports.ChatConversation
	flushed bool
}

func (c *reviewFlushConversation) QueryDeliveryAttempt(context.Context, ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	return domain.ChatDeliveryReceipt{}, ports.ErrChatDeliveryUnverified
}
func (c *reviewFlushConversation) FenceAndFlushOutbound(_ context.Context, keys []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	if len(keys) != 0 {
		return nil, errors.New("reviewer host received worker delivery keys")
	}
	c.flushed = true
	return nil, nil
}
func TestReviewerShutdownFlushesOwnHostWithoutWorkerLedger(t *testing.T) {
	conv := &reviewFlushConversation{}
	c := &Controller{reviewID: "review", store: &reviewDeliveryStore{}, conv: conv, state: ports.ChatControllerReady}
	if err := c.PreflightShutdown(t.Context()); err != nil {
		t.Fatalf("reviewer consulted worker ledger: %v", err)
	}
	if !conv.flushed {
		t.Fatal("reviewer skipped its native host flush")
	}
}
