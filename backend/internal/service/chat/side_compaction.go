package chat

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (m *sideManager) clearCompactionAttention(ctx context.Context, runtime *sideRuntime) {
	if err := outboundAttention(ctx, m.service.store, runtime.side.SessionID, "side:"+runtime.side.ID, true, m.service.now()); err != nil {
		m.service.log.Warn("side outbound attention cleanup failed", "side", runtime.side.ID, "code", "CHAT_ATTENTION_CLEANUP_FAILED")
	}
}

// One original command remains reserved until its native evidence converges.
// Intake can request this check without replaying the command or its questions.
func (m *sideManager) reconcileCompaction(ctx context.Context, runtime *sideRuntime, force bool) bool {
	runtime.compactionCheckMu.Lock()
	defer runtime.compactionCheckMu.Unlock()
	runtime.mu.Lock()
	id, epoch := runtime.compactionID, runtime.compactionEpoch
	pending := id != "" && runtime.outboundUnverified && (!runtime.compacting || force) && runtime.activeTurnID == "" && !runtime.dispatchBlocked
	due := force || !m.service.now().Before(runtime.nextCompactionCheck)
	if !pending || !due {
		runtime.mu.Unlock()
		return false
	}
	runtime.nextCompactionCheck = m.service.now().Add(30 * time.Second)
	runtime.mu.Unlock()
	ownerCtx := runtime.ctx
	if ownerCtx == nil {
		ownerCtx = m.ctx
	}
	checkCtx, cancel := context.WithTimeout(ownerCtx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	if checkCtx.Err() != nil {
		return false
	}
	if err := outboundAttention(checkCtx, m.service.store, runtime.side.SessionID, "side:"+runtime.side.ID, false, m.service.now()); err != nil {
		m.service.log.Warn("side compaction diagnosis unavailable", "side", runtime.side.ID, "code", "CHAT_ATTENTION_CREATE_FAILED")
	}
	reconciler, ok := runtime.conv.(ports.ChatOutboundReconciler)
	if !ok || epoch == "" || reconciler.OutboundProviderEpoch() != epoch || reconciler.ReconcileOutbound(checkCtx) != nil {
		return false
	}
	runtime.mu.Lock()
	settled := runtime.compactionID == id && runtime.activeTurnID == "" && !runtime.dispatchBlocked
	if settled {
		runtime.compacting = false
		runtime.compactionID = ""
		runtime.compactionEpoch = ""
		m.setOutboundUnverifiedLocked(runtime, false)
	}
	runtime.mu.Unlock()
	if settled {
		m.clearCompactionAttention(checkCtx, runtime)
		m.announce(runtime.side.ID)
		m.signal()
	}
	return settled
}
