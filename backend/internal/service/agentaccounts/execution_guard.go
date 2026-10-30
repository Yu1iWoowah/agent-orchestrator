package agentaccounts

import (
	"context"
	"slices"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type profileMutationContextKey struct{}

func (s *Service) acquireProfileRead(ctx context.Context, id string) (func(), error) {
	if owned, _ := ctx.Value(profileMutationContextKey{}).(string); owned == id && id != "" {
		return func() {}, nil
	}
	return s.AcquireAccountUses(ctx, id)
}

func (s *Service) accountUseLock(id string) *sync.RWMutex {
	s.useMu.Lock()
	defer s.useMu.Unlock()
	if s.uses == nil {
		s.uses = make(map[string]*sync.RWMutex)
	}
	if s.uses[id] == nil {
		s.uses[id] = &sync.RWMutex{}
	}
	return s.uses[id]
}

// AcquireAccountUses never waits behind a mutator while holding a session gate.
// Lifecycle operations likewise try their exclusive lock, avoiding lock-order
// cycles with stop-agent callbacks. Durable owners protect already-live agents;
// these short-lived holds close the preflight/Create/ownership-commit gaps.
func (s *Service) AcquireAccountUses(ctx context.Context, ids ...string) (func(), error) {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	var held []*sync.RWMutex
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, lock := range held {
				lock.RUnlock()
			}
		})
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		if id == "" || id == domain.DeviceAgentAccountID(domain.AgentAccountProviderClaude) || id == domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex) {
			continue
		}
		lock := s.accountUseLock(id)
		if !lock.TryRLock() {
			release()
			return nil, ports.ErrAgentAccountInUse
		}
		held = append(held, lock)
		// Read-only catalog stores cannot start a lifecycle operation. The
		// production SQLite store implements the journal used by all mutators.
		if _, journaled := s.store.(ports.AgentAccountJournal); !journaled {
			continue
		}
		if _, active, err := s.activeOperation(ctx, id); err != nil || active {
			release()
			if err != nil {
				return nil, err
			}
			if account, findErr := s.find(ctx, id); findErr == nil {
				if unavailable := accountAvailability(account); unavailable != nil {
					return nil, unavailable
				}
			}
			return nil, ports.ErrAgentAccountOperationBusy
		}
	}
	return release, nil
}

func (s *Service) acquireProfileMutation(id string) (func(), error) {
	lock := s.accountUseLock(id)
	if !lock.TryLock() {
		return nil, ports.ErrAgentAccountInUse
	}
	return lock.Unlock, nil
}
