package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// SetProjectAgentAccount is a narrow, serialized mutation: clients never read
// a whole config and overwrite unrelated settings with a stale partial copy.
// The same transaction selects a reviewer by stable harness, never array index.
func (s *Store) SetProjectAgentAccount(ctx context.Context, id, account string, provider domain.AgentAccountProvider, role, reviewer string, fallback domain.AgentHarness) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	updated := false
	err := s.inTx(ctx, "set project account", func(q *gen.Queries) error {
		stored, err := q.GetProject(ctx, domain.ProjectID(id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		row := projectRowFromGen(stored)
		if !row.ArchivedAt.IsZero() {
			return nil
		}
		cfg := row.Config
		matches := func(h domain.AgentHarness) bool {
			p, ok := domain.AgentAccountProviderFor(h)
			return ok && p == provider
		}
		switch role {
		case "":
			if reviewer != "" {
				return ports.ErrAgentAccountScopeInvalid
			}
			if cfg.AgentAccounts == nil {
				cfg.AgentAccounts = map[domain.AgentAccountProvider]string{}
			}
			cfg.AgentAccounts[provider] = account
		case "worker", "orchestrator":
			if reviewer != "" {
				return ports.ErrAgentAccountScopeInvalid
			}
			r := &cfg.Worker
			if role == "orchestrator" {
				r = &cfg.Orchestrator
			}
			h := r.Harness
			if h == "" {
				h = fallback
			}
			// An omitted harness inherits the daemon's configured role agent.
			// Never invent or rewrite that agent here; execution still validates
			// the resulting exact provider before starting a writer.
			if !matches(h) {
				return ports.ErrAgentAccountProviderMismatch
			}
			r.AgentAccountID = account
		case "reviewer":
			if reviewer == "" {
				return ports.ErrAgentAccountScopeInvalid
			}
			found := false
			for i := range cfg.Reviewers {
				if string(cfg.Reviewers[i].Harness) == reviewer {
					if !matches(domain.AgentHarness(reviewer)) {
						return ports.ErrAgentAccountProviderMismatch
					}
					cfg.Reviewers[i].AgentAccountID = account
					found = true
				}
			}
			if !found {
				return ports.ErrAgentAccountScopeInvalid
			}
		default:
			return ports.ErrAgentAccountScopeInvalid
		}
		config, err := marshalProjectConfig(cfg)
		if err != nil {
			return err
		}
		rows, err := q.UpdateProjectSettings(ctx, gen.UpdateProjectSettingsParams{ID: domain.ProjectID(id), DisplayName: row.DisplayName, Config: config})
		updated = rows > 0
		return err
	})
	return updated, err
}
