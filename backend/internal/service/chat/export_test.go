package chat

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// SetNativeHistoryLoadAttemptLimit shortens the per-attempt provider load bound
// for tests and returns a restore function.
func SetNativeHistoryLoadAttemptLimit(limit time.Duration) (restore func()) {
	previous := nativeHistoryLoadAttemptLimit
	nativeHistoryLoadAttemptLimit = limit
	return func() { nativeHistoryLoadAttemptLimit = previous }
}

// SetRenderMeasureTimeout shortens how long publishing waits for heights, for
// tests, and returns a restore function.
func SetRenderMeasureTimeout(timeout time.Duration) (restore func()) {
	previous := renderMeasureTimeout
	renderMeasureTimeout = timeout
	return func() { renderMeasureTimeout = previous }
}

// WaitRenderMeasures waits for the background measures PublishRender started.
func (s *Service) WaitRenderMeasures() { s.renderMeasures.Wait() }

// SetDeltaFlushInterval changes how long streamed deltas may wait to be batched
// for controllers started afterwards, and returns a restore function. Zero writes
// every delta on its own, the way the projector worked before batching.
func SetDeltaFlushInterval(interval time.Duration) (restore func()) {
	previous := deltaFlushInterval
	deltaFlushInterval = interval
	return func() { deltaFlushInterval = previous }
}

// DeltaRunMaxBytes is the most delta text one batched write carries.
const DeltaRunMaxBytes = deltaRunMaxBytes

// SendMuHeldForTest reports whether command dispatch is locked without waiting.
func (c *Controller) SendMuHeldForTest() bool {
	if !c.sendMu.TryLock() {
		return true
	}
	c.sendMu.Unlock()
	return false
}

// AdvanceDeliveryClockForTest cannot jump time between calculating the next
// delivery timer's delay and registering it with the injected clock.
func (c *Controller) AdvanceDeliveryClockForTest(advance func()) {
	c.deliveryCheckMu.Lock()
	defer c.deliveryCheckMu.Unlock()
	advance()
}

// DetachIncompleteForTest simulates a daemon disappearing without settling its
// provider work. Normal Close/StopAll must pass preflight instead. Keeping this
// in test code prevents an unverified normal-shutdown escape hatch.
func DetachIncompleteForTest(ctx context.Context, c *Controller) error {
	c.setOutboundQuiescing(true)
	return c.closeAfterPreflight(ctx)
}

// WaitBackgroundWakeForTest waits up to five seconds for the background wake
// of id to finish and reports whether it did.
func (s *Service) WaitBackgroundWakeForTest(id domain.SessionID) bool {
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.wakeMu.Lock()
		running := s.backgroundWakes[id]
		s.wakeMu.Unlock()
		if !running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}
