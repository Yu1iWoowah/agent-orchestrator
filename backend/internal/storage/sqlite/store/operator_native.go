package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func (s *Store) operatorIssuer() string {
	s.operatorIssuerMu.RLock()
	defer s.operatorIssuerMu.RUnlock()
	return s.operatorIssuerEpoch
}

// NativeOperatorIssuer is the bootstrap-only adapter passed to the private
// broker. Ordinary Store/domain ports deliberately have no mint method.
type NativeOperatorIssuer struct{ store *Store }

// NativeOperatorIssuer isolates bootstrap's private mint capability from the
// ordinary Store and domain ports. It must not be wired to HTTP controllers.
func (s *Store) NativeOperatorIssuer() *NativeOperatorIssuer {
	return &NativeOperatorIssuer{store: s}
}

// NextNativeOperatorAction selects one pending admitted plan for native review.
func (n *NativeOperatorIssuer) NextNativeOperatorAction(ctx context.Context, after domain.OperatorRequestRef) (domain.OperatorRequestRef, error) {
	s := n.store
	row, err := s.qr.NextNativeOperatorConfirmation(ctx, gen.NextNativeOperatorConfirmationParams{DaemonEpoch: s.operatorEpoch, NowMs: s.operatorNow().UnixMilli(), IssuerEpoch: s.operatorIssuer(), AfterRef: string(after)})
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrOperatorActionNotFound
	}
	return domain.OperatorRequestRef(row.RequestRef), err
}

// GetOperatorActionConsent is an internal domain port. An action handler may
// obtain the private reference only for its exact, admitted plan, then consume
// and reserve in its own transaction. Nothing is dispatched by native mint.
func (s *Store) GetOperatorActionConsent(ctx context.Context, plan domain.OperatorActionPlan) (domain.OperatorConsentRef, error) {
	_, digest, err := domain.CanonicalOperatorPlan(plan)
	if err != nil {
		return "", err
	}
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.qr.WithTx(tx)
	row, err := q.GetOperatorConfirmationByKey(ctx, plan.ActionKey)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrOperatorActionNotFound
	}
	if err != nil {
		return "", err
	}
	if row.PlanDigest != digest {
		return "", domain.ErrOperatorActionConflict
	}
	receipt, err := s.operatorReceipt(ctx, q, row)
	if err != nil {
		return "", err
	}
	if receipt.State != domain.OperatorActionConfirmed && receipt.State != domain.OperatorActionConsumed {
		return "", domain.ErrOperatorConsentUnavailable
	}
	consent, err := q.GetLiveOperatorConsent(ctx, row.RequestRef)
	if err != nil {
		return "", err
	}
	return domain.OperatorConsentRef(consent.ConsentRef), nil
}

// SetNativeOperatorIssuer is wired only to the private native protocol. Taking
// the writer lock serializes epoch replacement with mint and domain consume.
// An empty epoch disconnects the issuer; durable accepted actions remain valid.
func (n *NativeOperatorIssuer) SetNativeOperatorIssuer(ctx context.Context, epoch string) error {
	s := n.store
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	s.operatorIssuerMu.Lock()
	defer s.operatorIssuerMu.Unlock()
	s.operatorIssuerEpoch = epoch
	return nil
}

// NativeOperatorPlan returns only a fixed, live domain-admitted plan. A native
// review cannot rearm a stale daemon plan or extend its domain authority.
func (n *NativeOperatorIssuer) NativeOperatorPlan(ctx context.Context, ref domain.OperatorRequestRef) (domain.OperatorActionPlan, error) {
	s := n.store
	if !domain.OperatorReferenceValid(string(ref)) {
		return domain.OperatorActionPlan{}, domain.ErrOperatorPlanInvalid
	}
	row, err := s.qr.GetOperatorConfirmationByRef(ctx, string(ref))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.OperatorActionPlan{}, domain.ErrOperatorActionNotFound
	}
	if err != nil {
		return domain.OperatorActionPlan{}, err
	}
	if row.DaemonEpoch != s.operatorEpoch || row.RevokedAtMs.Valid || row.ValidUntilMs <= s.operatorNow().UnixMilli() {
		return domain.OperatorActionPlan{}, domain.ErrOperatorConsentUnavailable
	}
	var plan domain.OperatorActionPlan
	if json.Unmarshal([]byte(row.PlanJson), &plan) != nil {
		return plan, domain.ErrOperatorPlanInvalid
	}
	_, digest, err := domain.CanonicalOperatorPlan(plan)
	if err != nil || digest != row.PlanDigest {
		return domain.OperatorActionPlan{}, domain.ErrOperatorPlanInvalid
	}
	return plan, nil
}

// ConfirmNativeOperatorPlan is called only after the private issuer verifies
// its in-memory challenge and native dialog response. It repeats the immutable
// plan, epoch, revocation and time checks inside the mint transaction.
func (n *NativeOperatorIssuer) ConfirmNativeOperatorPlan(ctx context.Context, ref domain.OperatorRequestRef, digest, epoch string) (domain.OperatorConsentRef, error) {
	s := n.store
	if err := s.writeMu.LockContext(ctx); err != nil {
		return "", err
	}
	defer s.writeMu.Unlock()
	if epoch == "" || s.operatorIssuer() != epoch {
		return "", domain.ErrOperatorConsentUnavailable
	}
	var result domain.OperatorConsentRef
	err := s.inTxDB(ctx, "confirm native operator plan", func(q *gen.Queries, raw gen.DBTX) error {
		row, err := q.GetOperatorConfirmationByRef(ctx, string(ref))
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrOperatorActionNotFound
		}
		if err != nil {
			return err
		}
		now := s.operatorNow().UnixMilli()
		if row.PlanDigest != digest {
			return domain.ErrOperatorActionConflict
		}
		if row.DaemonEpoch != s.operatorEpoch || row.RevokedAtMs.Valid {
			return domain.ErrOperatorConsentUnavailable
		}
		consent, err := q.GetLiveOperatorConsent(ctx, row.RequestRef)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && consent.ConsumedAtMs.Valid {
			result = domain.OperatorConsentRef(consent.ConsentRef)
			return nil
		}
		if row.ValidUntilMs <= now {
			return domain.ErrOperatorConsentUnavailable
		}
		if err == nil && consent.IssuerEpoch == epoch && consent.DaemonEpoch == s.operatorEpoch && consent.ExpiresAtMs > now {
			result = domain.OperatorConsentRef(consent.ConsentRef)
			return nil
		}
		if err := q.InvalidateUnusedOperatorConsents(ctx, gen.InvalidateUnusedOperatorConsentsParams{RequestRef: row.RequestRef, InvalidatedAtMs: sql.NullInt64{Int64: now, Valid: true}}); err != nil {
			return err
		}
		result = domain.OperatorConsentRef(uuid.NewString())
		expires := min(row.ValidUntilMs, now+domain.OperatorConsentLifetime.Milliseconds())
		_, err = raw.ExecContext(ctx, `INSERT INTO operator_consents(consent_ref,request_ref,plan_digest,daemon_epoch,issuer_epoch,minted_at_ms,expires_at_ms) VALUES(?,?,?,?,?,?,?)`, string(result), row.RequestRef, digest, s.operatorEpoch, epoch, now, expires)
		return err
	})
	if err != nil {
		return "", err
	}
	return result, nil
}
