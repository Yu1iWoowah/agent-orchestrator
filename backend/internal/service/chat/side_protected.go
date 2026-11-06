package chat

import (
	"context"
	"runtime"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
)

// ProtectedSide exposes metadata only, even when durable content cannot be read.
type ProtectedSide struct {
	Protection domain.SideProtection      `json:"protection"`
	SideID     string                     `json:"sideId"`
	SessionID  domain.SessionID           `json:"sessionId"`
	Provider   domain.AgentHarness        `json:"provider"`
	State      domain.SideCompactionState `json:"state"`
	Historical bool                       `json:"historical"`
	Orphan     bool                       `json:"orphan"`
}

func (s *Service) protectedSideOrphan(ctx context.Context, sessionID domain.SessionID) (bool, error) {
	if s.sessions == nil {
		return true, nil
	}
	parent, found, err := s.sessions.GetSession(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return !found || parent.IsTerminated || domain.NormalizeSessionMode(parent.Mode) != domain.SessionModeChat, nil
}

// ListProtectedSides spans every desktop launch and does not require a parent.
func (s *Service) ListProtectedSides(ctx context.Context) ([]ProtectedSide, error) {
	out := []ProtectedSide{}
	if s.nativeGuard.store == nil {
		return out, nil
	}
	rows, err := s.nativeGuard.store.SideCompactions(ctx, true)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.SideID] {
			continue
		}
		seen[row.SideID] = true
		orphan, err := s.protectedSideOrphan(ctx, row.SessionID)
		if err != nil {
			return nil, err
		}
		out = append(out, ProtectedSide{SideID: row.SideID, SessionID: row.SessionID, Provider: row.Provider, State: row.State, Historical: row.Historical, Orphan: orphan, Protection: sideProtection(ctx, row)})
	}
	return out, nil
}

func sideProtection(ctx context.Context, row domain.SideCompaction) domain.SideProtection {
	p := domain.SideProtection{Result: row.State, Historical: row.Historical, Closed: row.Closed, Isolated: row.Unresolved()}
	if !p.Isolated {
		return p
	}
	p.ShutdownBlocked = true
	if len(row.Content) == 0 {
		p.Reason = "side_content_not_durable"
		return p
	}
	if applicable, reason := sideCompactionApplicable(row, runtime.GOOS); !applicable {
		p.Reason = reason
		return p
	}
	ended, reason := processscope.Ended(ctx, row.Scope)
	p.ShutdownBlocked, p.Reason = !ended, reason
	return p
}

func (s *Service) sideProtection(ctx context.Context, sideID string) (*domain.SideProtection, error) {
	if s.nativeGuard.store == nil {
		return nil, nil
	}
	rows, err := s.nativeGuard.store.SideCompactions(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.SideID == sideID {
			p := sideProtection(ctx, row)
			return &p, nil
		}
	}
	return nil, nil
}

// CloseProtectedSide is the global user close endpoint; it bypasses launch ownership.
func (s *Service) CloseProtectedSide(ctx context.Context, id string) error {
	if s.nativeGuard.store == nil {
		return ErrSideClosed
	}
	rows, err := s.nativeGuard.store.SideCompactions(ctx, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.SideID != id {
			continue
		}
		side := domain.SideConversation{ID: id, SessionID: row.SessionID, ProviderHostID: row.ProviderHostID, ProviderForkID: row.NativeID, Harness: row.Provider}
		store, ok := s.sides.store.(*memorySideStore)
		if !ok {
			return ErrSideUnavailable
		}
		store.mu.Lock()
		if existing, ok := store.sides[id]; ok {
			side = existing
		} else {
			store.sides[id] = side
		}
		store.mu.Unlock()
		if err := s.closeSideCompactions(ctx, id); err != nil {
			return err
		}
		return s.closeSide(ctx, side)
	}
	return ErrSideClosed
}
