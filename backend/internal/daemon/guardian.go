package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	guardiansvc "github.com/aoagents/agent-orchestrator/backend/internal/service/guardian"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func startGuardianObservation(ctx context.Context, store *sqlite.Store, device usagesvc.SourceRoots, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	service := guardiansvc.New(store, guardiansvc.Config{Roots: func(ctx context.Context, o domain.GuardianOwner) ([]string, error) {
		if o.AccountID == domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex) {
			return []string{device.CodexSessions, device.CodexArchived}, nil
		}
		accounts, err := store.ListAgentAccounts(ctx)
		if err != nil {
			return nil, errors.New("guardian account unavailable")
		}
		for _, a := range accounts {
			if a.ID == o.AccountID && a.Provider == domain.AgentAccountProviderCodex && a.DisabledAt == nil && a.RetiredAt == nil && a.Home != "" {
				// The registered account's history links are the only allowed root links;
				// descendants are still confined by os.Root during discovery and reading.
				var roots []string
				for _, name := range []string{"sessions", "archived_sessions"} {
					if root, err := filepath.EvalSymlinks(filepath.Join(a.Home, name)); err == nil {
						roots = append(roots, root)
					}
				}
				return roots, nil
			}
		}
		return nil, errors.New("guardian account unavailable")
	}})
	go func() {
		defer close(done)
		run := func() {
			if err := service.Collect(ctx); err != nil && ctx.Err() == nil {
				log.Warn("guardian observation incomplete", "code", "collection_failed")
			}
		}
		run()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
	return done
}
