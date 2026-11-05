package chat

import (
	"context"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (m *sideManager) protectedSide(ctx context.Context, id string) (bool, error) {
	store, ok := m.store.(*memorySideStore)
	if !ok || store.durable == nil {
		return false, nil
	}
	rows, err := store.durable.SideCompactions(ctx, true)
	if err != nil {
		return true, err
	}
	for _, row := range rows {
		if id != "" && row.SideID == id {
			return true, nil
		}
	}
	return false, nil
}

// Retry can observe the original compaction host, never start a replacement.
func (m *sideManager) reconnectProtected(ctx context.Context, side domain.SideConversation) error {
	m.mu.Lock()
	existing := m.runtimes[side.ID] != nil || m.restoring[side.ID] != nil
	if existing {
		m.mu.Unlock()
		return nil
	}
	connectCtx, connectCancel := context.WithCancel(ctx)
	m.restoring[side.ID] = connectCancel
	m.mu.Unlock()
	defer func() { connectCancel(); m.mu.Lock(); delete(m.restoring, side.ID); m.mu.Unlock() }()
	if m.service.drivers == nil {
		return nil
	}
	rows, err := m.service.nativeGuard.store.SideCompactions(connectCtx, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.SideID != side.ID || row.Historical {
			continue
		}
		cfg, driver, err := m.service.sideLaunchConfig(side)
		if err != nil {
			return err
		}
		reconnector, ok := driver.(ports.ChatDriverReconnector)
		if !ok {
			return ErrNativeConversationProtected
		}
		queryCtx := ports.WithChatCompactionEpoch(ports.WithChatCompactionID(connectCtx, row.ID), row.Epoch)
		key := ports.ChatResumeConfig{ReconnectOnly: true, SessionID: domain.SessionID(row.ProviderHostID), ProviderConversationID: row.NativeID, DataDir: cfg.DataDir, WorkspacePath: cfg.WorkspacePath, Env: cfg.Env, Permissions: cfg.Permissions, ReadOnly: cfg.ReadOnly, Model: side.Model, Effort: side.Effort, ProviderScopeID: side.ID, ProviderIDsScoped: true}
		conv, err := reconnector.Reconnect(queryCtx, key)
		if err != nil {
			return err
		}
		// Only query the original result; this attachment never enters dispatch.
		readOnly := &protectedSideConversation{ChatConversation: conv}
		m.mu.Lock()
		if m.runtimes[side.ID] != nil || connectCtx.Err() != nil || m.ctx.Err() != nil {
			m.mu.Unlock()
			_ = readOnly.Close()
			if connectCtx.Err() != nil {
				return connectCtx.Err()
			}
			return m.ctx.Err()
		}
		watchCtx, cancel := context.WithCancel(m.ctx)
		runtime := &sideRuntime{side: side, conv: readOnly, ctx: watchCtx, cancel: cancel, compactionID: row.ID, compactionEpoch: row.Epoch, outboundUnverified: true, queryOnly: true}
		m.runtimes[side.ID] = runtime
		m.mu.Unlock()
		release := func() {
			cancel()
			m.mu.Lock()
			if m.runtimes[side.ID] == runtime {
				delete(m.runtimes, side.ID)
			}
			m.mu.Unlock()
			_ = readOnly.Close()
		}
		go func() {
			defer release()
			for {
				select {
				case <-watchCtx.Done():
					return
				case event, ok := <-conv.Events():
					if !ok {
						return
					}
					owner, sameEpoch := conv.(ports.ChatOutboundReconciler)
					if sameEpoch && owner.OutboundProviderEpoch() == row.Epoch && row.Epoch != "" && event.Compaction != nil && event.Compaction.CommandID == row.ID && event.CompactionID == row.ID && event.ProviderTurnID != "" && (event.Compaction.Outcome == ports.ChatCompactionSettled || event.Compaction.Outcome == ports.ChatCompactionNotSent) {
						runtime.compactionCheckMu.Lock()
						m.settleCompactionResult(watchCtx, runtime, event)
						runtime.compactionCheckMu.Unlock()
					}
				}
			}
		}()
		if activator, ok := conv.(ports.ChatLiveReconnectActivator); ok {
			if err := activator.ActivateLiveReconnect(queryCtx, row.ID); err != nil {
				release()
				return err
			}
		}
		return nil
	}
	return ErrNativeConversationProtected
}

// The original conversation remains accessible for result observation only.
type protectedSideConversation struct {
	ports.ChatConversation
	closeOnce sync.Once
	closeErr  error
}

func (c *protectedSideConversation) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.ChatConversation.Close() })
	return c.closeErr
}

func (*protectedSideConversation) SendTurn(context.Context, ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	return ports.ChatTurnRef{}, ErrNativeConversationProtected
}
func (*protectedSideConversation) Interrupt(context.Context, string) error {
	return ErrNativeConversationProtected
}
func (*protectedSideConversation) ResolveRequest(context.Context, string, ports.ChatDecision) error {
	return ErrNativeConversationProtected
}
func (*protectedSideConversation) Compact(context.Context) (ports.ChatCompactionResult, error) {
	return ports.ChatCompactionResult{}, ErrNativeConversationProtected
}
func (*protectedSideConversation) Rollback(context.Context, string) error {
	return ErrNativeConversationProtected
}
func (*protectedSideConversation) Fork(context.Context, *string) (string, error) {
	return "", ErrNativeConversationProtected
}
func (*protectedSideConversation) SetTitle(context.Context, string) error {
	return ErrNativeConversationProtected
}
func (*protectedSideConversation) Steer(context.Context, string, ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	return ports.ChatTurnRef{}, ErrNativeConversationProtected
}

func (c *protectedSideConversation) AcknowledgeProviderEvent(ctx context.Context, id string) error {
	if ack, ok := c.ChatConversation.(ports.ChatProviderEventAcknowledger); ok {
		return ack.AcknowledgeProviderEvent(ctx, id)
	}
	return nil
}
