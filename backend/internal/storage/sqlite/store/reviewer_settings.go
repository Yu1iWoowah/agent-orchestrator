package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

var _ ports.ReviewerAgentSettingsStore = (*Store)(nil)

// ReviewerAgentSettingsOperationByKey reads an immutable accepted intent. It
// must not be used as authorization for a new request.
func (s *Store) ReviewerAgentSettingsOperationByKey(ctx context.Context, reviewID, key string) (domain.ReviewerAgentSettingsOperation, bool, error) {
	var result domain.ReviewerAgentSettingsOperation
	raw, err := s.qr.GetReviewerAgentSettingsOperation(ctx, gen.GetReviewerAgentSettingsOperationParams{ReviewID: reviewID, IdempotencyKey: key})
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return result, false, ports.ErrAgentSettingsInvalid
	}
	return result, true, nil
}

func decodeReviewerSelection(raw string) (domain.AgentSettings, error) {
	var settings domain.AgentSettings
	if json.Unmarshal([]byte(raw), &settings) != nil || settings.Validate() != nil {
		return settings, ports.ErrAgentSettingsInvalid
	}
	return settings, nil
}

func reviewerAgentSettings(ctx context.Context, q *gen.Queries, id string) (domain.ReviewerAgentSettingsState, error) {
	var state domain.ReviewerAgentSettingsState
	r, err := q.GetReviewerAgentSettings(ctx, id)
	if err != nil {
		return state, settingsStoreError(err)
	}
	state.Review = domain.Review{ID: r.ID, SessionID: r.SessionID, ProjectID: r.ProjectID, Harness: r.Harness, PRURL: r.PRURL,
		ReviewerHandleID: r.ReviewerHandleID, AgentSessionID: r.AgentSessionID, ReviewerLaunchID: r.ReviewerLaunchID,
		ReviewerActivityState: domain.ActivityState(r.ReviewerActivityState), InterfaceMode: domain.ReviewerInterfaceMode(r.InterfaceMode),
		ProviderConversationID: r.ProviderConversationID, ControllerGeneration: r.ControllerGeneration, ControllerError: r.ControllerError,
		IsArchived: r.IsArchived, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	state.Owner = domain.ReviewerSettingsOwner{ReviewID: r.ID, AccountID: r.AccountID, Generation: r.ReviewerLaunchID, NativeID: r.AgentSessionID}
	if state.Review.InterfaceMode == domain.ReviewerInterfaceChat {
		state.Owner.Generation, state.Owner.NativeID = r.ControllerGeneration, r.ProviderConversationID
	}
	state.Revision, state.HasRunningPass = r.SettingsRevision.Int64, r.HasRunningPass != 0
	if r.SettingsJson.Valid {
		selection, err := decodeReviewerSelection(r.SettingsJson.String)
		if err != nil {
			return state, err
		}
		state.Settings = &selection
	}
	return state, nil
}

// ReviewerAgentSettings never fills missing legacy preferences from a worker.
func (s *Store) ReviewerAgentSettings(ctx context.Context, id string) (domain.ReviewerAgentSettingsState, error) {
	return reviewerAgentSettings(ctx, s.qr, id)
}

// InitializeReviewerAgentSettings records a qualified initial selection once.
// Subsequent triggers cannot replace it with changed project defaults.
func (s *Store) InitializeReviewerAgentSettings(ctx context.Context, id string, initial domain.AgentSettings, at time.Time) (domain.ReviewerAgentSettingsState, error) {
	var result domain.ReviewerAgentSettingsState
	if initial.Validate() != nil {
		return result, ports.ErrAgentSettingsInvalid
	}
	raw, err := json.Marshal(initial)
	if err != nil {
		return result, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return result, err
	}
	defer s.writeMu.Unlock()
	err = s.inTx(ctx, "initialize reviewer settings", func(q *gen.Queries) error {
		if err := q.InitializeReviewerAgentSettings(ctx, gen.InitializeReviewerAgentSettingsParams{ReviewID: id, SettingsJson: string(raw), RecordedAt: at}); err != nil {
			return err
		}
		var err error
		result, err = reviewerAgentSettings(ctx, q, id)
		if err == nil && result.Settings == nil {
			return ports.ErrAgentSettingsConflict
		}
		return err
	})
	return result, err
}

// Recheck accepted, non-secret authority on the writer transaction. Account
// binding belongs to the reviewer and is independently compared by the CAS.
func reviewerSettingsActor(ctx context.Context, q *gen.Queries, op domain.ReviewerAgentSettingsOperation) error {
	if op.Actor.Kind == domain.AgentSettingsActorLocal && op.Actor.SessionID == "" && op.Authority == (domain.AgentSettingsAuthority{}) {
		return nil
	}
	now := time.Now().Unix()
	if op.Actor.Kind != domain.AgentSettingsActorOrchestrator || op.Actor.SessionID == "" || op.Authority.IssuedAt > now || op.Authority.ExpiresAt <= now {
		return ports.ErrAgentSettingsGrantInvalid
	}
	actorRow, err := q.GetSession(ctx, op.Actor.SessionID)
	if err != nil {
		return ports.ErrAgentSettingsGrantInvalid
	}
	actor := getSessionRowToRecord(actorRow)
	if actor.Kind != domain.KindOrchestrator || !op.Authority.Matches(actor) {
		return ports.ErrAgentSettingsGrantInvalid
	}
	state, err := reviewerAgentSettings(ctx, q, op.Owner.ReviewID)
	if err != nil {
		return err
	}
	parentRow, err := q.GetSession(ctx, state.Review.SessionID)
	if err != nil {
		return ports.ErrAgentSettingsConflict
	}
	parent := getSessionRowToRecord(parentRow)
	if parent.ID == actor.ID || parent.Kind != domain.KindWorker || parent.IsTerminated || parent.ProjectID != actor.ProjectID || state.Review.ProjectID != actor.ProjectID {
		return ports.ErrAgentSettingsGrantInvalid
	}
	rows, err := q.ListSessionsByProject(ctx, &actor.ProjectID)
	if err != nil {
		return err
	}
	for _, other := range rows {
		if other.Kind == domain.KindOrchestrator && !other.IsTerminated && other.ID != actor.ID {
			return ports.ErrAgentSettingsGrantInvalid
		}
	}
	return nil
}

func reviewerSettingsPolicy(ctx context.Context, q *gen.Queries, op domain.ReviewerAgentSettingsOperation) error {
	if op.Actor.Kind == domain.AgentSettingsActorLocal {
		return nil
	}
	state, err := reviewerAgentSettings(ctx, q, op.Owner.ReviewID)
	if err != nil {
		return err
	}
	p, err := q.GetProject(ctx, state.Review.ProjectID)
	if err != nil {
		return ports.ErrAgentSettingsConflict
	}
	policy := unmarshalProjectConfig(p.Config).AgentSettingsPolicy
	if policy == nil || policy.Validate() != nil || !policy.AllowsReviewer(state.Review.Harness, op.Target) {
		return ports.ErrAgentSettingsConflict
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != op.PolicyRevision {
		return ports.ErrAgentSettingsConflict
	}
	return nil
}

func validateReviewerSettingsOperation(op domain.ReviewerAgentSettingsOperation) error {
	if op.BaseRevision < 0 || op.BaseRevision == math.MaxInt64 || op.Source == nil || op.Source.Validate() != nil || op.Target.Validate() != nil || op.CreatedAt.IsZero() || op.Phase != "pinned" || op.ApplyMode != "next-pass" {
		return ports.ErrAgentSettingsInvalid
	}
	for _, value := range []string{op.ID, op.Owner.ReviewID, op.Owner.AccountID, op.Owner.Generation, op.Owner.NativeID, op.IdempotencyKey, op.RequestFingerprint} {
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n") {
			return ports.ErrAgentSettingsInvalid
		}
	}
	return nil
}

// CommitReviewerAgentSettings atomically pins the preference and immutable
// audit. No runtime is changed, and an active review or Chat turn is a conflict.
func (s *Store) CommitReviewerAgentSettings(ctx context.Context, op domain.ReviewerAgentSettingsOperation) (domain.ReviewerAgentSettingsOperation, error) {
	var result domain.ReviewerAgentSettingsOperation
	if op.Phase == "" {
		op.Phase = "pinned"
	}
	if op.ApplyMode == "" {
		op.ApplyMode = "next-pass"
	}
	if err := validateReviewerSettingsOperation(op); err != nil {
		return result, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return result, err
	}
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "commit reviewer settings", func(q *gen.Queries) error {
		if err := reviewerSettingsActor(ctx, q, op); err != nil {
			return err
		}
		raw, err := q.GetReviewerAgentSettingsOperation(ctx, gen.GetReviewerAgentSettingsOperationParams{ReviewID: op.Owner.ReviewID, IdempotencyKey: op.IdempotencyKey})
		if err == nil {
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return ports.ErrAgentSettingsInvalid
			}
			if result.RequestFingerprint != op.RequestFingerprint || result.Actor != op.Actor {
				return ports.ErrAgentSettingsConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := reviewerSettingsPolicy(ctx, q, op); err != nil {
			return err
		}
		source, err := json.Marshal(op.Source)
		if err != nil {
			return err
		}
		target, err := json.Marshal(op.Target)
		if err != nil {
			return err
		}
		n, err := q.CommitReviewerAgentSettings(ctx, gen.CommitReviewerAgentSettingsParams{ReviewID: op.Owner.ReviewID, BaseRevision: op.BaseRevision,
			SourceJson: string(source), TargetJson: string(target), AccountID: op.Owner.AccountID, Generation: op.Owner.Generation, NativeID: op.Owner.NativeID, RecordedAt: op.CreatedAt})
		if err != nil {
			return err
		}
		if n != 1 {
			return ports.ErrAgentSettingsConflict
		}
		op.Revision = op.BaseRevision + 1
		rawOp, err := json.Marshal(op)
		if err != nil {
			return err
		}
		if err := q.InsertReviewerAgentSettingsOperation(ctx, gen.InsertReviewerAgentSettingsOperationParams{ID: op.ID, ReviewID: op.Owner.ReviewID, Revision: op.Revision,
			IdempotencyKey: op.IdempotencyKey, RequestFingerprint: op.RequestFingerprint, OperationJson: string(rawOp), CreatedAt: op.CreatedAt}); err != nil {
			if isSQLiteUnique(err) {
				return ports.ErrAgentSettingsConflict
			}
			return err
		}
		result = op
		return nil
	})
	return result, err
}
