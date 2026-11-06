package chat

import (
	"context"
	"errors"
	"runtime"
	"sort"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Applicability uses the original launch facts; account ownership, native
// store location and provider sandboxing do not restrict policy B.
func sideCompactionApplicable(row domain.SideCompaction, platform string) (bool, string) {
	if platform != "linux" {
		return false, "scope_platform_unsupported"
	}
	if row.Provider == domain.HarnessCodex && !row.ReadOnly && row.PermissionMode != domain.PermissionModeAcceptEdits && row.PermissionMode != domain.PermissionModeAuto {
		return false, "codex_full_access"
	}
	return true, ""
}

// Flush before inspecting the original scope. The returned sides remain
// protected; only their shutdown veto is lifted.
func (m *sideManager) preservedCompactionSides(ctx context.Context, sessionID domain.SessionID) (map[string]bool, error) {
	preserved := map[string]bool{}
	store, ok := m.store.(*memorySideStore)
	if !ok || store.durable == nil {
		return preserved, nil
	}
	if err := store.loadCompactions(ctx); err != nil {
		return nil, errors.Join(ports.ErrChatShutdownNotSettled, err)
	}
	rows, err := store.durable.SideCompactions(ctx, true)
	if err != nil {
		return nil, errors.Join(ports.ErrChatShutdownNotSettled, err)
	}
	for _, row := range rows {
		if sessionID != "" && row.SessionID != sessionID {
			continue
		}
		store.mu.Lock()
		raw, flushErr := store.contentLocked(row.SideID)
		if flushErr == nil {
			flushErr = store.durable.SaveSideCompactionContent(ctx, row.SideID, raw)
		}
		store.mu.Unlock()
		if flushErr != nil {
			return nil, m.service.protectedSideShutdownError(ctx, row, "side_content_not_durable")
		}
		applicable, reason := sideCompactionApplicable(row, runtime.GOOS)
		if !applicable {
			return nil, m.service.protectedSideShutdownError(ctx, row, reason)
		}
		ended, reason, err := markEndedCompaction(ctx, store.durable, row)
		if !ended || err != nil {
			return nil, m.service.protectedSideShutdownError(ctx, row, reason)
		}
		preserved[row.SideID] = true
	}
	return preserved, nil
}

func (s *Service) protectedSideShutdownError(ctx context.Context, row domain.SideCompaction, reason string) error {
	orphan, err := s.protectedSideOrphan(ctx, row.SessionID)
	if err != nil {
		return errors.Join(ports.ErrChatShutdownNotSettled, err)
	}
	return &ports.ChatShutdownSideError{SideID: row.SideID, Reason: reason, Orphan: orphan}
}

// ShutdownPreservedSides reports the last successful fenced preflight.
func (s *Service) ShutdownPreservedSides() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string{}, s.shutdownPreservedSides...)
}

func sortedPreservedSides(sides map[string]bool) []string {
	ids := make([]string, 0, len(sides))
	for id := range sides {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
