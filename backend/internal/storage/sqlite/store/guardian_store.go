package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// GuardianOwners resolves only current durable account/native bindings. No new
// random epoch is minted: the natural identity is binding + account bound_at.
func (s *Store) GuardianOwners(ctx context.Context, at time.Time) ([]domain.GuardianOwner, error) {
	at = at.UTC()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out []domain.GuardianOwner
	err := s.inTx(ctx, "guardian owners", func(q *gen.Queries) error {
		rows, err := q.ListGuardianCandidates(ctx)
		if err != nil {
			return err
		}
		out = make([]domain.GuardianOwner, 0, len(rows))
		for _, r := range rows {
			if r.ProjectID == nil || !domain.ValidGuardianID(r.NativeRootID) || !domain.ValidGuardianID(r.AccountID) {
				continue
			}
			e, err := q.EnsureGuardianEpoch(ctx, gen.EnsureGuardianEpochParams{BindingID: r.BindingID, SessionID: string(r.SessionID), AccountID: r.AccountID, AccountBoundAt: r.BoundAt, FirstSeenAt: at})
			if err != nil {
				return err
			}
			out = append(out, domain.GuardianOwner{OwnershipConflict: r.OwnerConflict != 0, EpochID: e.ID, BindingID: r.BindingID, SessionID: r.SessionID, ProjectID: *r.ProjectID, AccountID: r.AccountID, AccountBoundAt: r.BoundAt, NativeRootID: r.NativeRootID, EnabledAt: r.EnabledAt, CleanedBefore: r.CleanedBefore.Time})
		}
		return nil
	})
	return out, err
}

// GuardianCursor reads the live thread cursor or returns an initial position.
func (s *Store) GuardianCursor(ctx context.Context, epoch int64, thread string) (domain.GuardianCursor, error) {
	r, err := s.qr.GetGuardianCursor(ctx, gen.GetGuardianCursorParams{EpochID: epoch, ThreadID: thread})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.GuardianCursor{EpochID: epoch, ThreadID: thread}, nil
	}
	return domain.GuardianCursor{EpochID: r.EpochID, ThreadID: r.ThreadID, Generation: r.Generation, Offset: r.ByteOffset, FileIdentity: r.FileIdentity, Discarding: r.Discarding != 0, UpdatedAt: r.UpdatedAt}, err
}

// ApplyGuardianBatch commits metadata and its independent cursor atomically.
// Fixed errors contain no provider text, paths, IDs or raw record fragments.
func (s *Store) ApplyGuardianBatch(ctx context.Context, b domain.GuardianBatch) error {
	b.Cursor.UpdatedAt = b.Cursor.UpdatedAt.UTC()
	if !domain.ValidGuardianID(b.Thread.ThreadID) || !domain.ValidGuardianID(b.Thread.ParentID) || !domain.ValidGuardianVersion(b.Thread.CLIVersion) || b.Cursor.Generation < 0 || b.Cursor.Offset < 0 || len(b.Cursor.FileIdentity) > 128 {
		return errors.New("invalid guardian batch")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "guardian batch", func(q *gen.Queries) error {
		policy, err := q.GuardianEpochEnabled(ctx, b.Owner.EpochID)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrGuardianDisabled
		}
		if err != nil {
			return err
		}
		if policy.Enabled != 1 {
			return domain.ErrGuardianDisabled
		}
		candidates, err := q.ListGuardianCandidates(ctx)
		if err != nil {
			return err
		}
		current := false
		for _, r := range candidates {
			if r.BindingID == b.Owner.BindingID && r.AccountID == b.Owner.AccountID && r.BoundAt.Equal(b.Owner.AccountBoundAt) && r.NativeRootID == b.Owner.NativeRootID && r.SessionID == b.Owner.SessionID && r.OwnerConflict == 0 {
				current = true
				break
			}
		}
		if !current {
			return domain.ErrGuardianDisabled
		}
		cur, err := q.GetGuardianCursor(ctx, gen.GetGuardianCursorParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if cur.ByteOffset != b.Expected.Offset || cur.Generation != b.Expected.Generation || cur.FileIdentity != b.Expected.FileIdentity || (cur.Discarding != 0) != b.Expected.Discarding || !cur.UpdatedAt.Equal(b.Expected.UpdatedAt) {
			return domain.ErrGuardianCursorConflict
		}

		position, err := q.GetGuardianPosition(ctx, gen.GetGuardianPositionParams{EpochID: b.Owner.EpochID, FileIdentity: b.Cursor.FileIdentity})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if position.ByteOffset != b.ExpectedPosition.Offset || position.Generation != b.ExpectedPosition.Generation || position.FileIdentity != b.ExpectedPosition.FileIdentity || (position.Discarding != 0) != b.ExpectedPosition.Discarding || !position.UpdatedAt.Equal(b.ExpectedPosition.UpdatedAt) {
			return domain.ErrGuardianCursorConflict
		}

		scope := b.Owner
		scope.EnabledAt = policy.EnabledAt
		scope.CleanedBefore = policy.CleanedBefore.Time

		models := make([]domain.GuardianTurnModel, 0, len(b.Models))
		turns := make(map[string]struct{}, len(b.Models)+len(b.Requests))
		for _, m := range b.Models {
			if scope.AcceptsProviderTime(m.ProviderAt, b.Cursor.UpdatedAt) {
				models = append(models, m)
			}
		}
		requests := make([]domain.GuardianRequest, 0, len(b.Requests))
		for _, r := range b.Requests {
			if scope.AcceptsProviderTime(r.ProviderAt, b.Cursor.UpdatedAt) {
				requests = append(requests, r)
			}
		}
		b.Models = models
		b.Requests = requests
		if cur.FileIdentity == "" && len(b.Models) == 0 && len(b.Requests) == 0 {
			for code, n := range b.Anomalies {
				if err := addGuardianAnomaly(ctx, q, b.Owner.SessionID, code, n, b.Cursor.UpdatedAt); err != nil {
					return err
				}
			}
			return putGuardianPosition(ctx, q, b)
		}

		old, err := q.GetGuardianThread(ctx, gen.GetGuardianThreadParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (old.ParentID != b.Thread.ParentID || (old.CliVersion != "" && old.CliVersion != b.Thread.CLIVersion)) {
			return addGuardianAnomaly(ctx, q, b.Owner.SessionID, domain.GuardianSourceConflict, 1, b.Cursor.UpdatedAt)
		}
		retentionAt := b.Thread.ProviderAt.UTC()
		if retentionAt.IsZero() || retentionAt.After(b.Cursor.UpdatedAt) {
			retentionAt = b.Cursor.UpdatedAt
		}
		if err := q.PutGuardianThread(ctx, gen.PutGuardianThreadParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, ParentID: b.Thread.ParentID, CliVersion: b.Thread.CLIVersion, RetentionAt: retentionAt, FirstSeenAt: b.Cursor.UpdatedAt, LastSeenAt: b.Cursor.UpdatedAt}); err != nil {
			return err
		}
		for _, m := range b.Models {
			m.ProviderAt = m.ProviderAt.UTC()
			if !domain.ValidGuardianID(m.TurnID) || !domain.ValidGuardianModel(m.ModelID) {
				return errors.New("invalid guardian model")
			}
			if !scope.AcceptsProviderTime(m.ProviderAt, b.Cursor.UpdatedAt) {
				continue
			}
			turns[m.TurnID] = struct{}{}
			n, err := q.PutGuardianModel(ctx, gen.PutGuardianModelParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, TurnID: m.TurnID, ModelID: m.ModelID, ProviderAt: m.ProviderAt})
			if err != nil {
				return err
			}
			if n == 0 {
				if err := addGuardianAnomaly(ctx, q, b.Owner.SessionID, domain.GuardianCoverageGap, 1, b.Cursor.UpdatedAt); err != nil {
					return err
				}
			}
		}
		for _, r := range b.Requests {
			r.ProviderAt = r.ProviderAt.UTC()
			if !domain.ValidGuardianID(r.ResponseID) || !domain.ValidGuardianID(r.TurnID) || (r.RootTurnID != "" && !domain.ValidGuardianID(r.RootTurnID)) || !r.Tokens.Valid() {
				return errors.New("invalid guardian request")
			}
			if !scope.AcceptsProviderTime(r.ProviderAt, b.Cursor.UpdatedAt) {
				continue
			}
			turns[r.TurnID] = struct{}{}
			if err := q.InsertGuardianRequest(ctx, gen.InsertGuardianRequestParams{CliVersion: b.Thread.CLIVersion, EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, TurnID: r.TurnID, ResponseID: r.ResponseID, RootTurnID: sql.NullString{String: r.RootTurnID, Valid: r.RootTurnID != ""}, InputTokens: guardianNull(r.Tokens.Input), CachedTokens: guardianNull(r.Tokens.Cached), CacheWriteTokens: guardianNull(r.Tokens.CacheWrite), OutputTokens: guardianNull(r.Tokens.Output), ReasoningTokens: guardianNull(r.Tokens.Reasoning), TotalTokens: guardianNull(r.Tokens.Total), ProviderAt: r.ProviderAt, FirstSeenAt: b.Cursor.UpdatedAt, SourceGeneration: r.SourceGeneration, SourceOffset: r.SourceOffset}); err != nil {
				return err
			}
		}
		for turn := range turns {
			if err := q.FillGuardianModels(ctx, gen.FillGuardianModelsParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, TurnID: turn}); err != nil {
				return err
			}
		}
		// A bounded turn-context queue includes already-resolved turns so a late
		// conflicting context cannot silently change a model. Excess is coverage_gap.
		for code, n := range b.Anomalies {
			if err := addGuardianAnomaly(ctx, q, b.Owner.SessionID, code, n, b.Cursor.UpdatedAt); err != nil {
				return err
			}
		}
		if err := q.PutGuardianCursor(ctx, gen.PutGuardianCursorParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, Generation: b.Cursor.Generation, ByteOffset: b.Cursor.Offset, FileIdentity: b.Cursor.FileIdentity, Discarding: guardianBool(b.Cursor.Discarding), UpdatedAt: b.Cursor.UpdatedAt}); err != nil {
			return err
		}
		return putGuardianPosition(ctx, q, b)
	})
}

func guardianNull(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}
func guardianPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}
func addGuardianAnomaly(ctx context.Context, q *gen.Queries, session domain.SessionID, code domain.GuardianCode, n int64, at time.Time) error {
	return q.AddGuardianAnomaly(ctx, gen.AddGuardianAnomalyParams{SessionID: string(session), Code: string(code), Count: n, FirstSeenAt: at.UTC()})
}

// RecordGuardianAnomaly increments a fixed diagnostic only while opted in.
func (s *Store) RecordGuardianAnomaly(ctx context.Context, id domain.SessionID, code domain.GuardianCode, at time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return addGuardianAnomaly(ctx, s.qw, id, code, 1, at)
}

// GuardianRequests returns the whitelist only; it cannot expose rollout paths
// or provider payloads. PR2 will add the user-facing query DTO and pagination.
func (s *Store) GuardianRequests(ctx context.Context, epoch int64) ([]domain.GuardianRequest, error) {
	rows, err := s.qr.ListGuardianRequests(ctx, epoch)
	if err != nil {
		return nil, err
	}
	out := make([]domain.GuardianRequest, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.GuardianRequest{CLIVersion: r.CliVersion, EpochID: r.EpochID, ThreadID: r.ThreadID, TurnID: r.TurnID, ResponseID: r.ResponseID, RootTurnID: r.RootTurnID.String, ModelID: r.ModelID.String, Tokens: domain.GuardianTokens{Input: guardianPtr(r.InputTokens), Cached: guardianPtr(r.CachedTokens), CacheWrite: guardianPtr(r.CacheWriteTokens), Output: guardianPtr(r.OutputTokens), Reasoning: guardianPtr(r.ReasoningTokens), Total: guardianPtr(r.TotalTokens)}, ProviderAt: r.ProviderAt, FirstSeenAt: r.FirstSeenAt, SourceGeneration: r.SourceGeneration, SourceOffset: r.SourceOffset, Conflict: r.Conflict != 0})
	}
	return out, nil
}

// GuardianAnomalies returns fixed-code counts without provider text.
func (s *Store) GuardianAnomalies(ctx context.Context, id domain.SessionID) (map[domain.GuardianCode]int64, error) {
	rows, err := s.qr.ListGuardianAnomalies(ctx, string(id))
	out := map[domain.GuardianCode]int64{}
	for _, r := range rows {
		out[domain.GuardianCode(r.Code)] = r.Count
	}
	return out, err
}

// CleanupGuardian advances the replay floor before deleting any expiring IDs.
// It also runs while collection is disabled; no ledger or transcript is touched.
func (s *Store) CleanupGuardian(ctx context.Context, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	cutoff := now.UTC().Add(-domain.GuardianRetention)
	return s.inTx(ctx, "guardian cleanup", func(q *gen.Queries) error {
		for _, f := range []func(context.Context, time.Time) error{q.AdvanceAllGuardianWatermarks, q.DeleteExpiredGuardianRequests, q.DeleteExpiredGuardianModels, q.ClearExpiredGuardianThreadMetadata, q.DeleteExpiredGuardianThreads, q.DeleteExpiredGuardianPositions, q.DeleteExpiredGuardianEpochs, q.DeleteExpiredGuardianAnomalies} {
			if err := f(ctx, cutoff); err != nil {
				return err
			}
		}
		return nil
	})
}

// PurgeGuardian deletes one session's observation facts and advances its replay floor.
func (s *Store) PurgeGuardian(ctx context.Context, id domain.SessionID, at time.Time) error {
	at = at.UTC()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "guardian purge", func(q *gen.Queries) error {
		if err := q.AdvanceGuardianWatermark(ctx, gen.AdvanceGuardianWatermarkParams{SessionID: string(id), CleanedBefore: at}); err != nil {
			return err
		}
		if err := q.PurgeGuardianEpochs(ctx, string(id)); err != nil {
			return err
		}
		return q.PurgeGuardianAnomalies(ctx, string(id))
	})
}

// GuardianCursors lists live native-thread cursors for missing-source detection.
func (s *Store) GuardianCursors(ctx context.Context, epoch int64) ([]domain.GuardianCursor, error) {
	rows, err := s.qr.ListGuardianCursors(ctx, epoch)
	out := make([]domain.GuardianCursor, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.GuardianCursor{EpochID: r.EpochID, ThreadID: r.ThreadID, Generation: r.Generation, Offset: r.ByteOffset, FileIdentity: r.FileIdentity, Discarding: r.Discarding != 0, UpdatedAt: r.UpdatedAt})
	}
	return out, err
}

func guardianBool(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

func putGuardianPosition(ctx context.Context, q *gen.Queries, b domain.GuardianBatch) error {
	return q.PutGuardianPosition(ctx, gen.PutGuardianPositionParams{EpochID: b.Owner.EpochID, FileIdentity: b.Cursor.FileIdentity, Generation: b.Cursor.Generation, ByteOffset: b.Cursor.Offset, Discarding: guardianBool(b.Cursor.Discarding), UpdatedAt: b.Cursor.UpdatedAt})
}

// GuardianPosition reads a durable file position that contains no native ID.
func (s *Store) GuardianPosition(ctx context.Context, epoch int64, identity string) (domain.GuardianCursor, error) {
	r, err := s.qr.GetGuardianPosition(ctx, gen.GetGuardianPositionParams{EpochID: epoch, FileIdentity: identity})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.GuardianCursor{EpochID: epoch}, nil
	}
	return domain.GuardianCursor{EpochID: r.EpochID, Generation: r.Generation, Offset: r.ByteOffset, FileIdentity: r.FileIdentity, Discarding: r.Discarding != 0, UpdatedAt: r.UpdatedAt}, err
}
