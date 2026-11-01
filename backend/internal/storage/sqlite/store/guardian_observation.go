package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ObserveGuardianAnomalies records transitions rather than polling frequency.
// An unchanged observation takes only a read connection; the transaction checks
// again so concurrent collectors cannot count the same source state twice.
func (s *Store) ObserveGuardianAnomalies(ctx context.Context, owner domain.GuardianOwner, source domain.GuardianSourceState, codes map[domain.GuardianCode]int64, at time.Time) error {
	if len(source.FileIdentity) > 128 || source.Size < 0 || source.Offset < 0 || source.Generation < 0 {
		return errors.New("invalid guardian source state")
	}
	rows, err := s.qr.ListGuardianAnomalyStates(ctx, gen.ListGuardianAnomalyStatesParams{EpochID: owner.EpochID, FileIdentity: source.FileIdentity})
	if err != nil {
		return err
	}
	if sameGuardianObservation(rows, source, codes) {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "guardian observation", func(q *gen.Queries) error {
		policy, err := q.GuardianEpochEnabled(ctx, owner.EpochID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && policy.Enabled != 1 {
			return domain.ErrGuardianDisabled
		}
		if err != nil {
			return err
		}
		if err := checkGuardianOwner(ctx, q, owner, true); err != nil {
			return err
		}
		return observeGuardianAnomalies(ctx, q, owner, source, codes, at)
	})
}

func sameGuardianObservation(rows []gen.GuardianAnomalyState, source domain.GuardianSourceState, codes map[domain.GuardianCode]int64) bool {
	active := 0
	for _, count := range codes {
		if count > 0 {
			active++
		}
	}
	if len(rows) != active {
		return false
	}
	for _, r := range rows {
		if codes[domain.GuardianCode(r.Code)] <= 0 || !sameGuardianSource(r, source) {
			return false
		}
	}
	return true
}

func sameGuardianSource(r gen.GuardianAnomalyState, s domain.GuardianSourceState) bool {
	return r.SourceSize == s.Size && r.SourceModifiedAt.Time.Equal(s.ModifiedAt) && r.SourceGeneration == s.Generation && r.SourceOffset == s.Offset
}

func observeGuardianAnomalies(ctx context.Context, q *gen.Queries, owner domain.GuardianOwner, source domain.GuardianSourceState, codes map[domain.GuardianCode]int64, at time.Time) error {
	rows, err := q.ListGuardianAnomalyStates(ctx, gen.ListGuardianAnomalyStatesParams{EpochID: owner.EpochID, FileIdentity: source.FileIdentity})
	if err != nil {
		return err
	}
	previous := make(map[domain.GuardianCode]gen.GuardianAnomalyState, len(rows))
	for _, r := range rows {
		code := domain.GuardianCode(r.Code)
		previous[code] = r
		if codes[code] <= 0 {
			if err := q.DeleteGuardianAnomalyState(ctx, gen.DeleteGuardianAnomalyStateParams{EpochID: owner.EpochID, FileIdentity: source.FileIdentity, Code: r.Code}); err != nil {
				return err
			}
		}
	}
	for code, count := range codes {
		if count <= 0 {
			continue
		}
		if old, ok := previous[code]; ok && sameGuardianSource(old, source) {
			continue
		}
		if err := q.PutGuardianAnomalyState(ctx, gen.PutGuardianAnomalyStateParams{EpochID: owner.EpochID, FileIdentity: source.FileIdentity, Code: string(code), SourceSize: source.Size, SourceModifiedAt: sql.NullTime{Time: source.ModifiedAt.UTC(), Valid: !source.ModifiedAt.IsZero()}, SourceGeneration: source.Generation, SourceOffset: source.Offset, FirstSeenAt: at.UTC()}); err != nil {
			return err
		}
		if err := addGuardianAnomaly(ctx, q, owner.SessionID, code, count, at); err != nil {
			return err
		}
	}
	return nil
}

func checkGuardianOwner(ctx context.Context, q *gen.Queries, owner domain.GuardianOwner, allowConflict bool) error {
	rows, err := q.ListGuardianCandidates(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.BindingID == owner.BindingID && r.AccountID == owner.AccountID && r.BoundAt.Equal(owner.AccountBoundAt) && r.NativeRootID == owner.NativeRootID && r.SessionID == owner.SessionID && (allowConflict || r.OwnerConflict == 0) {
			return nil
		}
	}
	return domain.ErrGuardianDisabled
}
