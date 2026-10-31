package guardian

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type checkpointKey struct {
	epoch  int64
	thread string
}
type checkpointProgress struct {
	identity   string
	generation int64
	next       int64
}

// checkpoints reads only retained request lines. A bounded rotating sample
// spreads validation across polls without replaying unrelated source payloads.
func (s *Service) checkpoints(ctx context.Context, f io.ReadSeeker, c domain.GuardianCursor) (bool, error) {
	key := checkpointKey{epoch: c.EpochID, thread: c.ThreadID}
	p := s.checks[key]
	if p.identity != c.FileIdentity || p.generation != c.Generation || p.next < c.ReadStartOffset || p.next >= c.Offset {
		p = checkpointProgress{identity: c.FileIdentity, generation: c.Generation, next: c.ReadStartOffset}
	}
	proofCursor := c
	proofCursor.ReadStartOffset = p.next
	rows, err := s.store.GuardianContinuityRequests(ctx, proofCursor, c.Offset, nil)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		p.next = c.ReadStartOffset
		s.checks[key] = p
		return false, nil
	}
	bytes := 0
	checked := 0
	for _, expected := range rows {
		if checked >= min(s.cfg.RecordBudget, 512) || bytes >= 4*maxLine {
			break
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, err := f.Seek(expected.SourceOffset, io.SeekStart); err != nil {
			return false, err
		}
		line, err := bufio.NewReaderSize(io.LimitReader(f, maxLine+1), 4096).ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		bytes += len(line)
		checked++
		actual, valid := requestMetadata(line, c.ThreadID)
		if errors.Is(err, io.EOF) || len(line) > maxLine || !valid || !sameRequestMetadata(expected, actual) {
			return true, nil
		}
		p.next = expected.SourceOffset + 1
	}
	if checked == len(rows) {
		p.next = c.ReadStartOffset
	}
	s.checks[key] = p
	return false, nil
}

// revalidationGap distinguishes newly recovered facts from a validation budget.
// Existing IDs need no write, and finishing only part of the interval is pending
// work rather than evidence of missing data.
func (s *Service) revalidationGap(ctx context.Context, b *domain.GuardianBatch, until int64) error {
	ids := make([]string, 0, len(b.Requests))
	for _, r := range b.Requests {
		if r.SourceOffset < until {
			ids = append(ids, r.ResponseID)
		}
	}
	if len(ids) == 0 {
		if len(b.Models) > 0 {
			exists, err := s.store.GuardianHasRequests(ctx, b.Owner.EpochID, b.Thread.ThreadID)
			if err != nil {
				return err
			}
			if !exists {
				b.Anomalies[domain.GuardianCoverageGap]++
			}
		}
		return nil
	}
	rows, err := s.store.GuardianContinuityRequests(ctx, b.Cursor, b.Cursor.ReadStartOffset, ids)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(rows))
	for _, r := range rows {
		known[r.ResponseID] = true
	}
	for _, id := range ids {
		if !known[id] {
			b.Anomalies[domain.GuardianCoverageGap]++
			break
		}
	}
	return nil
}

// Physical evidence decoding intentionally has no enable/TTL filters. Those
// gates still run in appendRecord and again inside the committing transaction.
func requestMetadata(line []byte, thread string) (domain.GuardianRequest, bool) {
	var r record
	if json.Unmarshal(line, &r) != nil || r.Type != "event_msg" || r.Payload.Type != "token_usage_record" || r.Timestamp.IsZero() || r.Payload.ThreadID != thread {
		return domain.GuardianRequest{}, false
	}
	p := r.Payload
	u := p.Usage
	tokens := domain.GuardianTokens{Input: u.Input, Cached: u.Cached, CacheWrite: u.CacheWrite, Output: u.Output, Reasoning: u.Reasoning, Total: u.Total}
	if !domain.ValidGuardianID(p.TurnID) || !domain.ValidGuardianID(p.ResponseID) || p.RootTurnID != "" && !domain.ValidGuardianID(p.RootTurnID) || !tokens.Valid() {
		return domain.GuardianRequest{}, false
	}
	return domain.GuardianRequest{ThreadID: thread, TurnID: p.TurnID, ResponseID: p.ResponseID, RootTurnID: p.RootTurnID, ProviderAt: r.Timestamp.UTC(), Tokens: tokens}, true
}

func sameRequestMetadata(a, b domain.GuardianRequest) bool {
	return a.ThreadID == b.ThreadID && a.TurnID == b.TurnID && a.ResponseID == b.ResponseID && a.RootTurnID == b.RootTurnID && a.ProviderAt.Equal(b.ProviderAt) && sameTokens(a.Tokens, b.Tokens)
}
