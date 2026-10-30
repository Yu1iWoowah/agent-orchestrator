package sessionguard

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestDeliveryOwnershipCheckRunsAfterComposerReadBeforeWrite(t *testing.T) {
	store := &fakeStore{rec: record(domain.ActivityIdle, false), ok: true}
	messenger := &fakeMessenger{}
	guard := New(store, messenger, nil)
	lease := &observedInputLease{acquired: make(chan struct{}), released: make(chan struct{})}
	guard.SetInputLease(lease)
	alive, released := true, false
	lost := errors.New("owned runtime exited during composer read")
	outcome, err := guard.DeliverWithOwnershipCheck(context.Background(), "s1", "handoff prompt", func(context.Context, domain.SessionRecord) error {
		if !isClosed(lease.acquired) || isClosed(lease.released) {
			t.Fatal("ownership check ran without the input lease")
		}
		if !alive {
			return lost
		}
		return nil
	}, func(context.Context, domain.SessionRecord) (bool, func()) {
		alive = false
		return false, func() { released = true }
	}, nil)
	if !errors.Is(err, lost) || outcome == Sent || len(messenger.sent) != 0 || !released || !isClosed(lease.released) {
		t.Fatalf("lost runtime received prompt or leaked fence: outcome=%v err=%v sent=%v released=%t", outcome, err, messenger.sent, released)
	}
}
