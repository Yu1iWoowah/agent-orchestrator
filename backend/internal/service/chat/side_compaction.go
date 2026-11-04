package chat

import (
	"context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (m *sideManager) clearCompactionAttention(ctx context.Context, runtime *sideRuntime) {
	if err := outboundAttention(ctx, m.service.store, runtime.side.SessionID, "side:"+runtime.side.ID, true, m.service.now()); err != nil {
		m.service.log.Warn("side outbound attention cleanup failed", "side", runtime.side.ID, "code", "CHAT_ATTENTION_CLEANUP_FAILED")
	}
}

// Only call after successful service-wide preflight while admission stays fenced.
// SideStore retains the original owners after their live runtimes are detached.
func (m *sideManager) clearSettledAttention(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sides, err := m.store.ListOpenSidesForRun(checkCtx, m.launchID())
	if err != nil {
		m.service.log.Warn("side attention cleanup unavailable", "code", "CHAT_ATTENTION_CLEANUP_FAILED")
		return
	}
	for _, side := range sides {
		if checkCtx.Err() != nil {
			return
		}
		if err := outboundAttention(checkCtx, m.service.store, side.SessionID, "side:"+side.ID, true, m.service.now()); err != nil {
			m.service.log.Warn("side outbound attention cleanup failed", "side", side.ID, "code", "CHAT_ATTENTION_CLEANUP_FAILED")
		}
	}
}

func (m *sideManager) diagnoseCompaction(ctx context.Context, runtime *sideRuntime) {
	if err := outboundAttention(ctx, m.service.store, runtime.side.SessionID, "side:"+runtime.side.ID, false, m.service.now()); err != nil {
		m.service.log.Warn("side compaction diagnosis unavailable", "side", runtime.side.ID, "code", "CHAT_ATTENTION_CREATE_FAILED")
	}
}

// One original command remains reserved until its native evidence converges.
// Intake can request this check without replaying the command or its questions.
func (m *sideManager) reconcileCompaction(ctx context.Context, runtime *sideRuntime, force bool) bool {
	runtime.compactionCheckMu.Lock()
	defer runtime.compactionCheckMu.Unlock()
	runtime.mu.Lock()
	id, epoch := runtime.compactionID, runtime.compactionEpoch
	pending := id != "" && !runtime.compactionDispatching && runtime.outboundUnverified && (!runtime.compacting || force) && runtime.activeTurnID == "" && !runtime.dispatchBlocked
	due := force || !m.service.now().Before(runtime.nextCompactionCheck)
	if !pending || !due {
		runtime.mu.Unlock()
		return false
	}
	runtime.nextCompactionCheck = m.service.now().Add(30 * time.Second)
	result := runtime.compactionResult
	runtime.mu.Unlock()
	if result != nil {
		if result.Compaction == nil || result.Compaction.CommandID != id {
			return false
		}
		return m.settleCompactionResult(ctx, runtime, *result)
	}
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
	m.diagnoseCompaction(checkCtx, runtime)
	reconciler, ok := runtime.conv.(ports.ChatOutboundReconciler)
	if !ok || epoch == "" || reconciler.OutboundProviderEpoch() != epoch || reconciler.ReconcileOutbound(checkCtx) != nil {
		return false
	}
	runtime.mu.Lock()
	settled := !runtime.compactionDispatching && runtime.compactionID == id && runtime.activeTurnID == "" && !runtime.dispatchBlocked
	if settled && m.transitionCompaction(checkCtx, runtime, domain.SideCompactionSettled) != nil {
		settled = false
	}
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

// settleCompactionResult consumes a validated original receipt while compactionCheckMu is held.
func (m *sideManager) settleCompactionResult(ctx context.Context, runtime *sideRuntime, event ports.ChatEvent) bool {
	state := domain.SideCompactionSettled
	if event.Compaction.Outcome == ports.ChatCompactionNotSent {
		state = domain.SideCompactionNotSent
	}
	if err := m.transitionCompaction(ctx, runtime, state); err != nil {
		runtime.mu.Lock()
		runtime.compacting = false
		runtime.compactionResult = &event
		runtime.mu.Unlock()
		m.service.log.Warn("side compaction result persistence failed", "side", runtime.side.ID, "code", "CHAT_SIDE_COMPACTION_RESULT_FAILED", "error", err)
		return false
	}
	if acknowledger, ok := runtime.conv.(ports.ChatProviderEventAcknowledger); ok && event.ProviderEventID != "" {
		ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := acknowledger.AcknowledgeProviderEvent(ackCtx, event.ProviderEventID)
		cancel()
		if err != nil {
			runtime.mu.Lock()
			runtime.dispatchBlocked = true
			runtime.mu.Unlock()
			m.failOpen(runtime.side, fmt.Errorf("side provider did not acknowledge compaction; close and reopen the side chat: %w", err))
		}
	}
	runtime.mu.Lock()
	runtime.compacting, runtime.compactionID, runtime.compactionEpoch, runtime.compactionResult = false, "", "", nil
	if !runtime.dispatchBlocked {
		m.setOutboundUnverifiedLocked(runtime, false)
	}
	settled := !runtime.dispatchBlocked
	runtime.mu.Unlock()
	if settled {
		m.clearCompactionAttention(ctx, runtime)
	}
	m.signal()
	return settled
}
