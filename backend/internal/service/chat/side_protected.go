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
	for _, candidate := range rows {
		if seen[candidate.SideID] {
			continue
		}
		seen[candidate.SideID] = true
		row, found, err := s.nativeGuard.store.UnresolvedSideCompactionWithContent(ctx, candidate.SideID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		orphan, err := s.protectedSideOrphan(ctx, row.SessionID)
		if err != nil {
			return nil, err
		}
		out = append(out, ProtectedSide{SideID: row.SideID, SessionID: row.SessionID, Provider: row.Provider, State: row.State, Historical: row.Historical, Orphan: orphan, Protection: s.sideProtectionStatus(ctx, row)})
	}
	return out, nil
}

func (s *Service) sideProtectionStatus(ctx context.Context, row domain.SideCompaction) domain.SideProtection {
	p := domain.SideProtection{Result: row.State, Historical: row.Historical, Closed: row.Closed, Isolated: row.Unresolved()}
	if !p.Isolated {
		return p
	}
	p.ShutdownBlocked = true
	p.Reason = "side_content_not_durable"
	if s.sides == nil {
		return p
	}
	store, ok := s.sides.store.(*memorySideStore)
	if !ok {
		return p
	}
	// Restore into a separate store so listing never changes live side state or
	// rewrites attachments, but still verifies the persisted content and images.
	projection := newMemorySideStore()
	projection.attachments = store.attachments
	if err := projection.restoreContentLocked(row.Content); err != nil || projection.sides[row.SideID].ID != row.SideID {
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
	row, found, err := s.nativeGuard.store.UnresolvedSideCompactionWithContent(ctx, sideID)
	if err != nil || !found {
		return nil, err
	}
	protection := s.sideProtectionStatus(ctx, row)
	return &protection, nil
}

// CloseProtectedSide is the global user close endpoint; it bypasses launch ownership.
func (s *Service) CloseProtectedSide(ctx context.Context, id string) error {
	if s.nativeGuard.store == nil {
		return ErrSideClosed
	}
	side, found, err := s.nativeGuard.store.UnresolvedSideForClose(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return ErrSideClosed
	}
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
