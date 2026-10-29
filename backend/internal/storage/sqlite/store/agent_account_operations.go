package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CreateAgentAccountOperation atomically records intent and disables new uses.
func (s *Store) CreateAgentAccountOperation(ctx context.Context, o domain.AgentAccountOperation) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "create account operation", func(q *gen.Queries) error {
		if err := q.CreateAgentAccountOperation(ctx, gen.CreateAgentAccountOperationParams{ID: o.ID, AccountID: o.AccountID, Provider: string(o.Provider), Action: o.Action, Phase: o.Phase, Outcome: o.Outcome, ReasonCode: o.ReasonCode, TerminalID: o.TerminalID, ResultAccountID: o.ResultAccountID, WasDisabled: o.WasDisabled, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}); err != nil {
			return err
		}
		if o.Action != "login" {
			return q.DisableAgentAccount(ctx, gen.DisableAgentAccountParams{ID: o.AccountID, DisabledAt: timeToNullTime(o.CreatedAt), LifecycleOperationID: o.ID})
		}
		return nil
	})
}
func updateAccountOperation(ctx context.Context, q *gen.Queries, o domain.AgentAccountOperation) error {
	if err := q.UpdateAgentAccountOperation(ctx, gen.UpdateAgentAccountOperationParams{ID: o.ID, Phase: o.Phase, Outcome: o.Outcome, ReasonCode: o.ReasonCode, ResultAccountID: o.ResultAccountID, UpdatedAt: o.UpdatedAt}); err != nil {
		return err
	}
	if o.Action == "remove" && o.Phase == "deleting" {
		if err := q.DeleteAgentAccountDefaultFor(ctx, o.AccountID); err != nil {
			return err
		}
		return q.DeleteAgentAccount(ctx, o.AccountID)
	}
	if o.Phase == "complete" && ((o.Action == "login" || o.Action == "login-again") && o.Outcome == "complete" || o.Action == "login-again" && o.Outcome == "cancelled" && !o.WasDisabled) {
		return q.EnableAgentAccount(ctx, o.AccountID)
	}
	return nil
}

// UpdateAgentAccountOperation commits the next checkpoint and its metadata effects.
func (s *Store) UpdateAgentAccountOperation(ctx context.Context, o domain.AgentAccountOperation) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "update account operation", func(q *gen.Queries) error { return updateAccountOperation(ctx, q, o) })
}

// CommitAgentAccountLogin accepts verified metadata without enabling a live login writer.
func (s *Store) CommitAgentAccountLogin(ctx context.Context, o domain.AgentAccountOperation, a domain.AgentAccount) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "accept account login", func(q *gen.Queries) error {
		kind := a.ProfileKind
		if kind == "" {
			kind = domain.AgentAccountProfileExternal
		}
		if err := q.UpsertAgentAccountLogin(ctx, gen.UpsertAgentAccountLoginParams{ID: a.ID, Provider: string(a.Provider), Home: a.Home, Email: a.Email, Plan: a.Plan, CreatedAt: a.CreatedAt, ProfileKind: string(kind), DisabledAt: timeToNullTime(o.UpdatedAt), LifecycleOperationID: o.ID}); err != nil {
			return err
		}
		return updateAccountOperation(ctx, q, o)
	})
}
func operationFromRow(o gen.AgentAccountOperation) domain.AgentAccountOperation {
	return domain.AgentAccountOperation{ID: o.ID, AccountID: o.AccountID, Provider: domain.AgentAccountProvider(o.Provider), Action: o.Action, Phase: o.Phase, Outcome: o.Outcome, ReasonCode: o.ReasonCode, TerminalID: o.TerminalID, ResultAccountID: o.ResultAccountID, WasDisabled: o.WasDisabled, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}
}

// GetAgentAccountOperation reads a non-secret durable lifecycle record.
func (s *Store) GetAgentAccountOperation(ctx context.Context, id string) (domain.AgentAccountOperation, bool, error) {
	o, e := s.qr.GetAgentAccountOperation(ctx, id)
	if errors.Is(e, sql.ErrNoRows) {
		return domain.AgentAccountOperation{}, false, nil
	}
	return operationFromRow(o), e == nil, e
}

// ListAgentAccountOperations returns lifecycle history for reconciliation.
func (s *Store) ListAgentAccountOperations(ctx context.Context) ([]domain.AgentAccountOperation, error) {
	rows, e := s.qr.ListAgentAccountOperations(ctx)
	if e != nil {
		return nil, e
	}
	out := make([]domain.AgentAccountOperation, 0, len(rows))
	for _, r := range rows {
		out = append(out, operationFromRow(r))
	}
	return out, nil
}
