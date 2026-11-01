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
	candidates, err := s.qr.ListGuardianCandidates(ctx)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	owners, missing, err := guardianOwners(ctx, s.qr, candidates, at, false)
	if err != nil || !missing {
		return owners, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err = s.inTx(ctx, "guardian owners", func(q *gen.Queries) error {
		rows, err := q.ListGuardianCandidates(ctx)
		if err != nil {
			return err
		}
		owners, _, err = guardianOwners(ctx, q, rows, at, true)
		return err
	})
	return owners, err
}

func guardianOwners(ctx context.Context, q *gen.Queries, rows []gen.ListGuardianCandidatesRow, at time.Time, create bool) ([]domain.GuardianOwner, bool, error) {
	out := make([]domain.GuardianOwner, 0, len(rows))
	for _, r := range rows {
		if r.ProjectID == nil || !domain.ValidGuardianID(r.NativeRootID) || !domain.ValidGuardianID(r.AccountID) {
			continue
		}
		e, err := q.GetGuardianEpoch(ctx, gen.GetGuardianEpochParams{BindingID: r.BindingID, AccountID: r.AccountID, AccountBoundAt: r.BoundAt})
		if errors.Is(err, sql.ErrNoRows) {
			if !create {
				return nil, true, nil
			}
			e, err = q.EnsureGuardianEpoch(ctx, gen.EnsureGuardianEpochParams{BindingID: r.BindingID, SessionID: string(r.SessionID), AccountID: r.AccountID, AccountBoundAt: r.BoundAt, FirstSeenAt: at})
		}
		if err != nil {
			return nil, false, err
		}
		out = append(out, domain.GuardianOwner{OwnershipConflict: r.OwnerConflict != 0, EpochID: e.ID, BindingID: r.BindingID, SessionID: r.SessionID, ProjectID: *r.ProjectID, AccountID: r.AccountID, AccountBoundAt: r.BoundAt, NativeRootID: r.NativeRootID, EnabledAt: r.EnabledAt, CleanedBefore: r.CleanedBefore.Time})
	}
	return out, false, nil
}

// GuardianCursor reads the live thread cursor or returns an initial position.
func (s *Store) GuardianCursor(ctx context.Context, epoch int64, thread string) (domain.GuardianCursor, error) {
	r, err := s.qr.GetGuardianCursor(ctx, gen.GetGuardianCursorParams{EpochID: epoch, ThreadID: thread})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.GuardianCursor{EpochID: epoch, ThreadID: thread}, nil
	}
	return guardianCursor(r), err
}

// ApplyGuardianBatch commits metadata and its independent cursor atomically.
// Fixed errors contain no provider text, paths, IDs or raw record fragments.
func (s *Store) ApplyGuardianBatch(ctx context.Context, b domain.GuardianBatch) error {
	b.Cursor.UpdatedAt = b.Cursor.UpdatedAt.UTC()
	if b.Anomalies == nil {
		b.Anomalies = make(map[domain.GuardianCode]int64)
	}
	if !domain.ValidGuardianID(b.Thread.ThreadID) || !domain.ValidGuardianID(b.Thread.ParentID) || !domain.ValidGuardianVersion(b.Thread.CLIVersion) || b.Cursor.ReplayOffset < 0 || b.Cursor.ReplayUntil < 0 || b.Cursor.ReadStartOffset < 0 || b.Cursor.ReadStartOffset > b.Cursor.Offset || b.Cursor.Generation < 0 || b.Cursor.Offset < 0 || len(b.Cursor.FileIdentity) > 128 {
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
		if err := checkGuardianOwner(ctx, q, b.Owner, false); err != nil {
			return err
		}

		cur, err := q.GetGuardianCursor(ctx, gen.GetGuardianCursorParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !guardianCursorMatches(guardianCursor(cur), b.Expected) {
			return domain.ErrGuardianCursorConflict
		}

		position, err := q.GetGuardianPosition(ctx, gen.GetGuardianPositionParams{EpochID: b.Owner.EpochID, FileIdentity: b.Cursor.FileIdentity})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !guardianCursorMatches(guardianPosition(position), b.ExpectedPosition) {
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
			if err := observeGuardianAnomalies(ctx, q, b.Owner, b.Cursor.SourceState(), b.Anomalies, b.Cursor.UpdatedAt); err != nil {
				return err
			}
			return putGuardianPosition(ctx, q, b)
		}

		old, err := q.GetGuardianThread(ctx, gen.GetGuardianThreadParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (old.ParentID != b.Thread.ParentID || (old.CliVersion != "" && old.CliVersion != b.Thread.CLIVersion)) {
			return observeGuardianAnomalies(ctx, q, b.Owner, b.Cursor.SourceState(), map[domain.GuardianCode]int64{domain.GuardianSourceConflict: 1}, b.Cursor.UpdatedAt)
		}
		retentionAt := b.Thread.ProviderAt.UTC()
		if retentionAt.IsZero() || retentionAt.After(b.Cursor.UpdatedAt) {
			retentionAt = b.Cursor.UpdatedAt
		}
		if errors.Is(err, sql.ErrNoRows) {
			if err := q.PutGuardianThread(ctx, gen.PutGuardianThreadParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, ParentID: b.Thread.ParentID, CliVersion: b.Thread.CLIVersion, RetentionAt: retentionAt, FirstSeenAt: b.Cursor.UpdatedAt, LastSeenAt: b.Cursor.UpdatedAt}); err != nil {
				return err
			}
		}
		changed := false
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
			changed = changed || n > 0
			if n == 0 {
				exists, err := q.GuardianModelExists(ctx, gen.GuardianModelExistsParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, TurnID: m.TurnID})
				if err != nil {
					return err
				}
				if !exists {
					b.Anomalies[domain.GuardianCoverageGap]++
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
			n, err := q.InsertGuardianRequest(ctx, gen.InsertGuardianRequestParams{CliVersion: b.Thread.CLIVersion, EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, TurnID: r.TurnID, ResponseID: r.ResponseID, RootTurnID: sql.NullString{String: r.RootTurnID, Valid: r.RootTurnID != ""}, InputTokens: guardianNull(r.Tokens.Input), CachedTokens: guardianNull(r.Tokens.Cached), CacheWriteTokens: guardianNull(r.Tokens.CacheWrite), OutputTokens: guardianNull(r.Tokens.Output), ReasoningTokens: guardianNull(r.Tokens.Reasoning), TotalTokens: guardianNull(r.Tokens.Total), ProviderAt: r.ProviderAt, FirstSeenAt: b.Cursor.UpdatedAt, SourceGeneration: r.SourceGeneration, SourceOffset: r.SourceOffset})
			if err != nil {
				return err
			}

			changed = changed || n > 0
			// The insert checks immutable metadata first. Compatible replays may
			// move their validation checkpoint without rewriting origin facts or CDC.
			if err := q.RefreshGuardianRequestCheckpoint(ctx, gen.RefreshGuardianRequestCheckpointParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, ResponseID: r.ResponseID, SourceGeneration: r.SourceGeneration, SourceOffset: r.SourceOffset, ProviderAt: r.ProviderAt}); err != nil {
				return err
			}
		}
		if changed {
			if err := q.TouchGuardianThread(ctx, gen.TouchGuardianThreadParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, LastSeenAt: b.Cursor.UpdatedAt}); err != nil {
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
		if err := observeGuardianAnomalies(ctx, q, b.Owner, b.Cursor.SourceState(), b.Anomalies, b.Cursor.UpdatedAt); err != nil {
			return err
		}
		if err := q.PutGuardianCursor(ctx, gen.PutGuardianCursorParams{EpochID: b.Owner.EpochID, ThreadID: b.Thread.ThreadID, Generation: b.Cursor.Generation, ByteOffset: b.Cursor.Offset, FileIdentity: b.Cursor.FileIdentity, ReplayUntil: b.Cursor.ReplayUntil, ReplayOffset: b.Cursor.ReplayOffset, ReplayDiscarding: guardianBool(b.Cursor.ReplayDiscarding), ReplayAgain: guardianBool(b.Cursor.ReplayAgain), ReadStartOffset: b.Cursor.ReadStartOffset, Discarding: guardianBool(b.Cursor.Discarding), SourceSize: b.Cursor.SourceSize, SourceModifiedAt: sql.NullTime{Time: b.Cursor.SourceModifiedAt, Valid: !b.Cursor.SourceModifiedAt.IsZero()}, UpdatedAt: b.Cursor.UpdatedAt}); err != nil {
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
		out = append(out, guardianRequest(r))
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
	hasState, err := s.qr.HasGuardianCleanupState(ctx)
	if err != nil {
		return err
	}
	if !hasState {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	cutoff := now.UTC().Add(-domain.GuardianRetention)
	return s.inTx(ctx, "guardian cleanup", func(q *gen.Queries) error {
		candidates, err := q.ListGuardianCandidates(ctx)
		if err != nil {
			return err
		}
		activeEpochs := make([]int64, 0, len(candidates))
		for _, c := range candidates {
			if c.ProjectID == nil || !domain.ValidGuardianID(c.NativeRootID) || !domain.ValidGuardianID(c.AccountID) {
				continue
			}
			e, err := q.GetGuardianEpoch(ctx, gen.GetGuardianEpochParams{BindingID: c.BindingID, AccountID: c.AccountID, AccountBoundAt: c.BoundAt})
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			activeEpochs = append(activeEpochs, e.ID)
		}
		deletePositions := func(ctx context.Context, at time.Time) error {
			return q.DeleteExpiredGuardianPositions(ctx, gen.DeleteExpiredGuardianPositionsParams{Cutoff: at, ActiveEpochIds: activeEpochs})
		}
		for _, f := range []func(context.Context, time.Time) error{q.AdvanceAllGuardianWatermarks, q.DeleteExpiredGuardianAnomalyStates, q.DeleteExpiredGuardianReviews, q.DeleteExpiredGuardianRequests, q.DeleteExpiredGuardianModels, q.ClearExpiredGuardianThreadMetadata, q.DeleteExpiredGuardianThreads, deletePositions, q.DeleteExpiredGuardianEpochs, q.DeleteExpiredGuardianAnomalies} {
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
		out = append(out, guardianCursor(r))
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
	return q.PutGuardianPosition(ctx, gen.PutGuardianPositionParams{EpochID: b.Owner.EpochID, FileIdentity: b.Cursor.FileIdentity, Generation: b.Cursor.Generation, ByteOffset: b.Cursor.Offset, ReplayUntil: b.Cursor.ReplayUntil, ReplayOffset: b.Cursor.ReplayOffset, ReplayDiscarding: guardianBool(b.Cursor.ReplayDiscarding), ReplayAgain: guardianBool(b.Cursor.ReplayAgain), ReadStartOffset: b.Cursor.ReadStartOffset, Discarding: guardianBool(b.Cursor.Discarding), SourceSize: b.Cursor.SourceSize, SourceModifiedAt: sql.NullTime{Time: b.Cursor.SourceModifiedAt, Valid: !b.Cursor.SourceModifiedAt.IsZero()}, UpdatedAt: b.Cursor.UpdatedAt})
}

// GuardianPosition reads a durable file position that contains no native ID.
func (s *Store) GuardianPosition(ctx context.Context, epoch int64, identity string) (domain.GuardianCursor, error) {
	r, err := s.qr.GetGuardianPosition(ctx, gen.GetGuardianPositionParams{EpochID: epoch, FileIdentity: identity})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.GuardianCursor{EpochID: epoch}, nil
	}
	return guardianPosition(r), err
}

func guardianRequest(r gen.GuardianRequest) domain.GuardianRequest {
	return domain.GuardianRequest{CLIVersion: r.CliVersion, EpochID: r.EpochID, ThreadID: r.ThreadID, TurnID: r.TurnID, ResponseID: r.ResponseID, RootTurnID: r.RootTurnID.String, ModelID: r.ModelID.String, Tokens: domain.GuardianTokens{Input: guardianPtr(r.InputTokens), Cached: guardianPtr(r.CachedTokens), CacheWrite: guardianPtr(r.CacheWriteTokens), Output: guardianPtr(r.OutputTokens), Reasoning: guardianPtr(r.ReasoningTokens), Total: guardianPtr(r.TotalTokens)}, ProviderAt: r.ProviderAt, FirstSeenAt: r.FirstSeenAt, SourceGeneration: r.SourceGeneration, SourceOffset: r.SourceOffset, Conflict: r.Conflict != 0}
}

// GuardianContinuityRequests reads bounded immutable metadata evidence without
// applying retention filters: expiry is not proof of a physical rewrite.
func (s *Store) GuardianContinuityRequests(ctx context.Context, c domain.GuardianCursor, end int64, ids []string) ([]domain.GuardianRequest, error) {
	rows, err := s.qr.ListGuardianContinuityRequests(ctx, gen.ListGuardianContinuityRequestsParams{EpochID: c.EpochID, ThreadID: c.ThreadID, SourceGeneration: c.Generation, StartOffset: c.ReadStartOffset, EndOffset: end, ResponseIds: ids})
	if err != nil {
		return nil, err
	}
	out := make([]domain.GuardianRequest, 0, len(rows))
	for _, r := range rows {
		proof := guardianRequest(r)
		proof.SourceGeneration = r.CheckpointGeneration
		proof.SourceOffset = r.CheckpointOffset
		out = append(out, proof)
	}
	return out, nil
}

// GuardianHasRequests reports retained request evidence for revalidation.
func (s *Store) GuardianHasRequests(ctx context.Context, epoch int64, thread string) (bool, error) {
	return s.qr.HasGuardianRequests(ctx, gen.HasGuardianRequestsParams{EpochID: epoch, ThreadID: thread})
}

func guardianCursor(r gen.GuardianCursor) domain.GuardianCursor {
	return domain.GuardianCursor{EpochID: r.EpochID, ThreadID: r.ThreadID, ReplayUntil: r.ReplayUntil, ReplayOffset: r.ReplayOffset, ReplayDiscarding: r.ReplayDiscarding != 0, ReplayAgain: r.ReplayAgain != 0, ReadStartOffset: r.ReadStartOffset, SourceSize: r.SourceSize, SourceModifiedAt: r.SourceModifiedAt.Time, Generation: r.Generation, Offset: r.ByteOffset, FileIdentity: r.FileIdentity, Discarding: r.Discarding != 0, UpdatedAt: r.UpdatedAt}
}

func guardianPosition(r gen.GuardianPosition) domain.GuardianCursor {
	return domain.GuardianCursor{EpochID: r.EpochID, ReplayUntil: r.ReplayUntil, ReplayOffset: r.ReplayOffset, ReplayDiscarding: r.ReplayDiscarding != 0, ReplayAgain: r.ReplayAgain != 0, ReadStartOffset: r.ReadStartOffset, SourceSize: r.SourceSize, SourceModifiedAt: r.SourceModifiedAt.Time, Generation: r.Generation, Offset: r.ByteOffset, FileIdentity: r.FileIdentity, Discarding: r.Discarding != 0, UpdatedAt: r.UpdatedAt}
}

func guardianCursorMatches(a, b domain.GuardianCursor) bool {
	// Keys are checked by the scoped queries; compare every durable state field.
	a.EpochID, b.EpochID = 0, 0
	a.ThreadID, b.ThreadID = "", ""
	a.UpdatedAt, b.UpdatedAt = a.UpdatedAt.UTC(), b.UpdatedAt.UTC()
	a.SourceModifiedAt, b.SourceModifiedAt = a.SourceModifiedAt.UTC(), b.SourceModifiedAt.UTC()
	return a == b
}
