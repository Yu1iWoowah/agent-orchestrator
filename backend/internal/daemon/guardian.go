package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
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
		return guardianRoots(ctx, store, device, o)
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

// guardianRoots resolves only an enabled registered account's existing history
// directories. Canonicalizing root links is required for managed shared history.
func guardianRoots(ctx context.Context, store *sqlite.Store, device usagesvc.SourceRoots, o domain.GuardianOwner) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.AccountID == domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex) {
		return guardianHistoryRoots([]string{device.CodexSessions, device.CodexArchived})
	}
	accounts, err := store.ListAgentAccounts(ctx)
	if err != nil {
		return nil, errors.New("guardian account unavailable")
	}
	for _, a := range accounts {
		if a.ID == o.AccountID && a.Provider == domain.AgentAccountProviderCodex && a.DisabledAt == nil && a.RetiredAt == nil && a.Home != "" {
			return guardianHistoryRoots([]string{filepath.Join(a.Home, "sessions"), filepath.Join(a.Home, "archived_sessions")})
		}
	}
	return nil, errors.New("guardian account unavailable")
}
func guardianHistoryRoots(candidates []string) ([]string, error) {
	roots := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Lstat(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			} // Optional history has not been created.
			return nil, errors.New("guardian account unavailable")
		}
		root, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return nil, errors.New("guardian account unavailable")
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return nil, errors.New("guardian account unavailable")
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil, errors.New("guardian account unavailable")
	}
	return roots, nil
}

func guardianQuery(store *sqlite.Store) *guardiansvc.QueryService {
	return guardiansvc.NewQuery(store, nil)
}
