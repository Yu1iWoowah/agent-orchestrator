package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// settingsRequest excludes timestamps, generated operation IDs, and phase from
// the idempotency identity. It contains only non-secret, validated input.
type settingsRequest struct {
	BaseRevision int64                     `json:"baseRevision"`
	Source       domain.AgentSettings      `json:"source"`
	Target       domain.AgentSettings      `json:"target"`
	AccountID    string                    `json:"accountId"`
	Generation   string                    `json:"generation"`
	Actor        domain.AgentSettingsActor `json:"actor"`
}

func requestForSettings(op domain.AgentSettingsOperation) settingsRequest {
	return settingsRequest{op.BaseRevision, op.Source, op.Target, op.AccountID, op.ControllerGeneration, op.Actor}
}

// SessionAgentSettings reads preferences and owner context from one snapshot.
func (s *Store) SessionAgentSettings(ctx context.Context, id domain.SessionID) (domain.SessionAgentSettings, error) {
	row, err := s.qr.GetSessionAgentSettings(ctx, id)
	if err != nil {
		return domain.SessionAgentSettings{}, settingsStoreError(err)
	}
	generation := row.RuntimeLaunchID
	if row.SessionMode == domain.SessionModeChat {
		generation = row.ControllerGeneration
	}
	return domain.SessionAgentSettings{SessionID: id, Harness: row.Harness, AccountID: row.AccountID, OwnerGeneration: generation, Settings: domain.AgentSettings{Model: domain.AgentModelSelection(row.Model), Effort: domain.AgentEffortSelection(row.Effort), FastMode: domain.AgentFastMode(row.AgentFastMode)}, Revision: row.AgentSettingsRevision}, nil
}

func settingsStoreError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ErrAgentSettingsNotFound
	}
	return err
}

func decodeSettingsOperation(row gen.SessionAgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	var req settingsRequest
	if err := json.Unmarshal([]byte(row.RequestJson), &req); err != nil {
		return domain.AgentSettingsOperation{}, fmt.Errorf("decode agent settings operation: %w", err)
	}
	return domain.AgentSettingsOperation{ID: row.ID, SessionID: domain.SessionID(row.SessionID), IdempotencyKey: row.IdempotencyKey,
		BaseRevision: req.BaseRevision, Source: req.Source, Target: req.Target, AccountID: req.AccountID, ControllerGeneration: req.Generation, Actor: req.Actor,
		Phase: domain.AgentSettingsPhase(row.Phase), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

// AgentSettingsOperation reads a session-owned durable operation.
func (s *Store) AgentSettingsOperation(ctx context.Context, id domain.SessionID, operationID string) (domain.AgentSettingsOperation, error) {
	row, err := s.qr.GetAgentSettingsOperation(ctx, gen.GetAgentSettingsOperationParams{SessionID: string(id), ID: operationID})
	if err != nil {
		return domain.AgentSettingsOperation{}, settingsStoreError(err)
	}
	return decodeSettingsOperation(row)
}

func validateSettingsOperation(op domain.AgentSettingsOperation) error {
	if op.ID == "" || op.SessionID == "" || op.IdempotencyKey == "" || len(op.ID) > 256 || len(op.IdempotencyKey) > 256 || op.BaseRevision < 0 || op.CreatedAt.IsZero() {
		return fmt.Errorf("invalid agent settings operation identity")
	}
	if err := op.Source.Validate(); err != nil {
		return err
	}
	if err := op.Target.Validate(); err != nil {
		return err
	}
	switch op.Actor.Kind {
	case domain.AgentSettingsActorLocal:
		if op.Actor.SessionID != "" {
			return fmt.Errorf("local actor cannot claim a session identity")
		}
	case domain.AgentSettingsActorOrchestrator:
		if op.Actor.SessionID == "" {
			return fmt.Errorf("orchestrator actor requires a session identity")
		}
	default:
		return fmt.Errorf("invalid agent settings actor")
	}
	if len(op.AccountID) > 256 || len(op.ControllerGeneration) > 256 || strings.ContainsAny(op.AccountID, "/\\\r\n") {
		return fmt.Errorf("invalid settings owner reference")
	}
	return nil
}

// BeginAgentSettingsOperation saves a pending intention and its audit together.
// No runtime is touched. Callers must authorize the actor before invoking it.
func (s *Store) BeginAgentSettingsOperation(ctx context.Context, op domain.AgentSettingsOperation) (domain.AgentSettingsOperation, error) {
	if err := validateSettingsOperation(op); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AgentSettingsOperation
	err := s.inTx(ctx, "begin agent settings", func(q *gen.Queries) error {
		row, err := q.GetAgentSettingsOperationByKey(ctx, gen.GetAgentSettingsOperationByKeyParams{SessionID: string(op.SessionID), IdempotencyKey: op.IdempotencyKey})
		if err == nil {
			existing, err := decodeSettingsOperation(row)
			if err != nil {
				return err
			}
			if requestForSettings(existing) != requestForSettings(op) {
				return ports.ErrAgentSettingsConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		current, err := q.GetSessionAgentSettings(ctx, op.SessionID)
		if err != nil {
			return settingsStoreError(err)
		}
		if !settingsOwnerMatches(current, op) {
			return ports.ErrAgentSettingsConflict
		}
		raw, err := json.Marshal(requestForSettings(op))
		if err != nil {
			return err
		}
		err = q.InsertAgentSettingsOperation(ctx, gen.InsertAgentSettingsOperationParams{ID: op.ID, SessionID: string(op.SessionID), IdempotencyKey: op.IdempotencyKey, RequestJson: string(raw), CreatedAt: op.CreatedAt, UpdatedAt: op.CreatedAt})
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return ports.ErrAgentSettingsConflict
			}
			return err
		}
		if err := q.InsertAgentSettingsAudit(ctx, gen.InsertAgentSettingsAuditParams{OperationID: op.ID, Phase: "pending", RecordedAt: op.CreatedAt}); err != nil {
			return err
		}
		result = op
		result.Phase = domain.AgentSettingsPending
		result.UpdatedAt = op.CreatedAt
		return nil
	})
	return result, err
}

func settingsOwnerMatches(row gen.GetSessionAgentSettingsRow, op domain.AgentSettingsOperation) bool {
	generation := row.RuntimeLaunchID
	if row.SessionMode == "chat" {
		generation = row.ControllerGeneration
	}
	return !row.IsTerminated && row.AgentSettingsRevision == op.BaseRevision && row.Model == string(op.Source.Model) && row.Effort == string(op.Source.Effort) && row.AgentFastMode == string(op.Source.FastMode) && generation == op.ControllerGeneration && row.AccountID == op.AccountID
}

// CommitAgentSettingsOperation updates the session, runtime projections and
// audit in one transaction. It is a persistence primitive, not a live setter.
func (s *Store) CommitAgentSettingsOperation(ctx context.Context, id domain.SessionID, operationID string, at time.Time) (domain.AgentSettingsOperation, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AgentSettingsOperation
	err := s.inTx(ctx, "commit agent settings", func(q *gen.Queries) error {
		row, err := q.GetAgentSettingsOperation(ctx, gen.GetAgentSettingsOperationParams{SessionID: string(id), ID: operationID})
		if err != nil {
			return settingsStoreError(err)
		}
		op, err := decodeSettingsOperation(row)
		if err != nil {
			return err
		}
		if op.Phase == domain.AgentSettingsApplied {
			result = op
			return nil
		}
		if op.Phase != domain.AgentSettingsPending {
			return ports.ErrAgentSettingsConflict
		}
		current, err := q.GetSessionAgentSettings(ctx, id)
		if err != nil {
			return settingsStoreError(err)
		}
		if !settingsOwnerMatches(current, op) {
			return ports.ErrAgentSettingsConflict
		}
		changed, err := q.CommitSessionAgentSettings(ctx, gen.CommitSessionAgentSettingsParams{Model: string(op.Target.Model), Effort: string(op.Target.Effort), FastMode: string(op.Target.FastMode), ID: id, ExpectedRevision: op.BaseRevision, SourceModel: string(op.Source.Model), SourceEffort: string(op.Source.Effort), SourceFastMode: string(op.Source.FastMode), ExpectedGeneration: op.ControllerGeneration})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ports.ErrAgentSettingsConflict
		}
		raw, err := q.GetSessionLaunchOptions(ctx, id)
		if err != nil {
			return err
		}
		var launch domain.AgentLaunchOptions
		if raw != "" {
			if err = json.Unmarshal([]byte(raw), &launch); err != nil {
				return fmt.Errorf("decode source launch options: %w", err)
			}
		}
		launch.FastMode = op.Target.FastMode == domain.AgentFastOn
		launch.FastModeOff = op.Target.FastMode == domain.AgentFastOff
		if err := q.SetSessionLaunchOptions(ctx, gen.SetSessionLaunchOptionsParams{LaunchOptions: encodeLaunchOptions(launch), ID: id}); err != nil {
			return err
		}
		if err := q.ProjectConversationAgentSettings(ctx, gen.ProjectConversationAgentSettingsParams{Model: string(op.Target.Model), Effort: string(op.Target.Effort), FastMode: string(op.Target.FastMode), UpdatedAt: at, SessionID: &id}); err != nil {
			return err
		}
		changed, err = q.SetAgentSettingsOperationPhase(ctx, gen.SetAgentSettingsOperationPhaseParams{Phase: "applied", UpdatedAt: at, SessionID: string(id), ID: operationID})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ports.ErrAgentSettingsConflict
		}
		if err := q.InsertAgentSettingsAudit(ctx, gen.InsertAgentSettingsAuditParams{OperationID: operationID, Phase: "applied", RecordedAt: at}); err != nil {
			return err
		}
		op.Phase = domain.AgentSettingsApplied
		op.UpdatedAt = at
		result = op
		return nil
	})
	return result, err
}
