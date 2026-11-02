package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

var _ ports.OperatorConsentStore = (*Store)(nil)

// PrepareOperatorAction saves a fixed, admitted plan without minting or executing.
func (s *Store) PrepareOperatorAction(ctx context.Context, plan domain.OperatorActionPlan) (domain.OperatorActionReceipt, error) {
	plan.ValidUntil = plan.ValidUntil.UTC().Truncate(time.Millisecond)
	encoded, digest, err := domain.CanonicalOperatorPlan(plan)
	if err != nil {
		return domain.OperatorActionReceipt{}, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.OperatorActionReceipt{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.OperatorActionReceipt
	err = s.inTx(ctx, "prepare operator action", func(q *gen.Queries) error {
		now := s.operatorNow().UnixMilli()
		row, err := q.GetOperatorConfirmationByKey(ctx, plan.ActionKey)
		if err == nil {
			if row.PlanDigest != digest {
				return domain.ErrOperatorActionConflict
			}
			receipt, err := s.operatorReceipt(ctx, q, row)
			if err != nil {
				return err
			}
			// A live request can need a new boot even when its old consent expired.
			// Consumed actions keep the original key/result across every restart.
			if receipt.State != domain.OperatorActionConsumed && !row.RevokedAtMs.Valid && row.ValidUntilMs > now &&
				(row.DaemonEpoch != s.operatorEpoch || receipt.State == domain.OperatorActionStale) {
				if err := q.InvalidateUnusedOperatorConsents(ctx, gen.InvalidateUnusedOperatorConsentsParams{RequestRef: row.RequestRef, InvalidatedAtMs: sql.NullInt64{Int64: now, Valid: true}}); err != nil {
					return err
				}
				if err := q.RearmOperatorConfirmation(ctx, gen.RearmOperatorConfirmationParams{RequestRef: row.RequestRef, DaemonEpoch: s.operatorEpoch}); err != nil {
					return err
				}
				row.DaemonEpoch = s.operatorEpoch
				receipt, err = s.operatorReceipt(ctx, q, row)
			}
			result = receipt
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if plan.ValidUntil.UnixMilli() <= now {
			return domain.ErrOperatorConsentUnavailable
		}
		ref := uuid.NewString()
		if err := q.InsertOperatorConfirmation(ctx, gen.InsertOperatorConfirmationParams{RequestRef: ref, ActionKey: plan.ActionKey, ProjectID: plan.Scope.ProjectRef, Purpose: string(plan.Purpose), PlanJson: encoded, PlanDigest: digest, DaemonEpoch: s.operatorEpoch, ValidUntilMs: plan.ValidUntil.UnixMilli(), CreatedAtMs: now}); err != nil {
			return err
		}
		row, err = q.GetOperatorConfirmationByKey(ctx, plan.ActionKey)
		if err != nil {
			return err
		}
		result, err = s.operatorReceipt(ctx, q, row)
		return err
	})
	if err != nil {
		return domain.OperatorActionReceipt{}, err
	}
	return result, nil
}

// GetOperatorAction reads a coherent receipt under its original action key.
func (s *Store) GetOperatorAction(ctx context.Context, actionKey string) (domain.OperatorActionReceipt, error) {
	if !domain.OperatorReferenceValid(actionKey) {
		return domain.OperatorActionReceipt{}, domain.ErrOperatorPlanInvalid
	}
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return domain.OperatorActionReceipt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.qr.WithTx(tx)
	row, err := q.GetOperatorConfirmationByKey(ctx, actionKey)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.OperatorActionReceipt{}, domain.ErrOperatorActionNotFound
	}
	if err != nil {
		return domain.OperatorActionReceipt{}, err
	}
	return s.operatorReceipt(ctx, q, row)
}

func (s *Store) operatorReceipt(ctx context.Context, q *gen.Queries, row gen.OperatorConfirmationRequest) (domain.OperatorActionReceipt, error) {
	issuer := s.operatorIssuer()
	result := domain.OperatorActionReceipt{RequestRef: domain.OperatorRequestRef(row.RequestRef), ActionKey: row.ActionKey, Purpose: domain.OperatorPurpose(row.Purpose), State: domain.OperatorActionPrepared, ValidUntil: time.UnixMilli(row.ValidUntilMs).UTC()}
	consent, err := q.GetLiveOperatorConsent(ctx, row.RequestRef)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.OperatorActionReceipt{}, err
	}
	now := s.operatorNow().UnixMilli()
	switch {
	case err == nil && consent.ConsumedAtMs.Valid:
		result.State, result.ReservationRef = domain.OperatorActionConsumed, consent.ReservationRef
	case row.RevokedAtMs.Valid:
		result.State = domain.OperatorActionRevoked
	case row.ValidUntilMs <= now || (err == nil && consent.ExpiresAtMs <= now):
		result.State = domain.OperatorActionExpired
	case row.DaemonEpoch != s.operatorEpoch:
		result.State = domain.OperatorActionStale
	case err == nil:
		switch {
		case consent.DaemonEpoch != s.operatorEpoch || consent.IssuerEpoch != issuer || issuer == "":
			result.State = domain.OperatorActionStale
		default:
			result.State = domain.OperatorActionConfirmed
		}
	}
	return result, nil
}

// RevokeUnused revokes pending authority; accepted actions remain domain-owned.
func (s *Store) RevokeUnused(ctx context.Context, ref domain.OperatorRequestRef) error {
	if !domain.OperatorReferenceValid(string(ref)) {
		return domain.ErrOperatorPlanInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "revoke operator action", func(q *gen.Queries) error {
		row, err := q.GetOperatorConfirmationByRef(ctx, string(ref))
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrOperatorActionNotFound
		}
		if err != nil {
			return err
		}
		receipt, err := s.operatorReceipt(ctx, q, row)
		if err != nil {
			return err
		}
		if receipt.State == domain.OperatorActionConsumed {
			return domain.ErrOperatorActionConflict
		}
		now := sql.NullInt64{Int64: s.operatorNow().UnixMilli(), Valid: true}
		if err := q.RevokeOperatorConfirmation(ctx, gen.RevokeOperatorConfirmationParams{RequestRef: row.RequestRef, RevokedAtMs: now}); err != nil {
			return err
		}
		return q.InvalidateUnusedOperatorConsents(ctx, gen.InvalidateUnusedOperatorConsentsParams{RequestRef: row.RequestRef, InvalidatedAtMs: now})
	})
}

// withOperatorConsent joins an adapter-owned transaction. Real consumer stores
// call it from inTx and reserve their own journal rows using that same q. Keeping
// construction here prevents callers from obtaining a standalone consume/commit.
// A failed consume poisons the scope even if a callback drops its error.
func (s *Store) withOperatorConsent(q *gen.Queries, fn func(ports.OperatorConsentTransaction) error) error {
	participant := &operatorConsentTx{store: s, q: q, active: true}
	defer func() { participant.active = false }()
	if err := fn(participant); err != nil {
		return err
	}
	return participant.failed
}

type operatorConsentTx struct {
	store  *Store
	q      *gen.Queries
	active bool
	failed error
}

var _ ports.OperatorConsentTransaction = (*operatorConsentTx)(nil)

func (tx *operatorConsentTx) ConsumeInActionTransaction(ctx context.Context, ref domain.OperatorConsentRef, plan domain.OperatorActionPlan, actionKey, reservationRef string) (result domain.OperatorConsumption, err error) {
	defer func() {
		if err != nil {
			tx.failed = err
		}
	}()
	if !tx.active || tx.failed != nil {
		return result, domain.ErrOperatorConsentUnavailable
	}
	_, digest, err := domain.CanonicalOperatorPlan(plan)
	if err != nil {
		return result, err
	}
	if plan.ActionKey != actionKey || !domain.OperatorReferenceValid(reservationRef) {
		return result, domain.ErrOperatorActionConflict
	}
	consent, err := tx.q.GetOperatorConsentByRef(ctx, string(ref))
	if errors.Is(err, sql.ErrNoRows) {
		return result, domain.ErrOperatorConsentUnavailable
	}
	if err != nil {
		return result, err
	}
	row, err := tx.q.GetOperatorConfirmationByKey(ctx, actionKey)
	if errors.Is(err, sql.ErrNoRows) {
		return result, domain.ErrOperatorActionConflict
	}
	if err != nil {
		return result, err
	}
	if consent.RequestRef != row.RequestRef || row.PlanDigest != digest || consent.PlanDigest != digest {
		return result, domain.ErrOperatorActionConflict
	}
	if consent.InvalidatedAtMs.Valid || row.RevokedAtMs.Valid {
		return result, domain.ErrOperatorConsentUnavailable
	}
	result = domain.OperatorConsumption{RequestRef: domain.OperatorRequestRef(row.RequestRef), ActionKey: actionKey, PlanDigest: digest, ReservationRef: reservationRef}
	if consent.ConsumedAtMs.Valid {
		if consent.ReservationRef != reservationRef {
			return domain.OperatorConsumption{}, domain.ErrOperatorActionConflict
		}
		result.Replay = true
		return result, nil
	}
	now := tx.store.operatorNow().UnixMilli()
	if row.DaemonEpoch != tx.store.operatorEpoch || consent.DaemonEpoch != tx.store.operatorEpoch || consent.IssuerEpoch == "" || consent.IssuerEpoch != tx.store.operatorIssuer() || consent.MintedAtMs > now || consent.ExpiresAtMs <= now || row.ValidUntilMs <= now || consent.ExpiresAtMs > row.ValidUntilMs {
		return domain.OperatorConsumption{}, domain.ErrOperatorConsentUnavailable
	}
	changed, err := tx.q.ConsumeOperatorConsent(ctx, gen.ConsumeOperatorConsentParams{ConsentRef: string(ref), ConsumedAtMs: sql.NullInt64{Int64: now, Valid: true}, ReservationRef: reservationRef})
	if err != nil {
		return domain.OperatorConsumption{}, err
	}
	if changed != 1 {
		return domain.OperatorConsumption{}, domain.ErrOperatorActionConflict
	}
	return result, nil
}
