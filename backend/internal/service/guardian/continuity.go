package guardian

import (
	"bufio"
	"context"
	"encoding/json"
	"io"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type prefixRecord struct {
	offset  int64
	request domain.GuardianRequest
	valid   bool
}

// replayPrefix reprocesses the entire claimed read interval when it fits the
// same bounded record/byte budgets as ingestion. Unverified suffixes are gaps.
// Replaying the typed whitelist catches new facts hidden between checkpoints;
// SQL dedupe preserves first_seen/retention and makes unchanged replays silent.
func (s *Service) replayPrefix(ctx context.Context, f io.ReadSeeker, b *domain.GuardianBatch, sourceChanged bool) (bool, error) {
	start, stop := b.Cursor.ReadStartOffset, b.Cursor.Offset
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return false, err
	}
	reader := bufio.NewReaderSize(io.LimitReader(f, min(stop-start, 4*maxLine)), maxLine+1)
	probe := domain.GuardianBatch{Owner: b.Owner, Thread: b.Thread, Cursor: b.Cursor, Anomalies: map[domain.GuardianCode]int64{}}
	entries := make([]prefixRecord, 0, min(s.cfg.RecordBudget, 512))
	ids := make([]string, 0, min(s.cfg.RecordBudget, 512))
	offset := start
	for n := 0; n < s.cfg.RecordBudget && offset < stop; n++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		line, err := reader.ReadSlice('\n')
		if err != nil || len(line) > maxLine {
			break
		}
		request, valid := requestMetadata(line, b.Thread.ThreadID)
		entries = append(entries, prefixRecord{offset: offset, request: request, valid: valid})
		if valid {
			ids = append(ids, request.ResponseID)
		}
		appendRecord(&probe, line, offset)
		offset += int64(len(line))
	}
	gap := offset < stop
	proofs, err := s.store.GuardianContinuityRequests(ctx, b.Cursor, offset, ids)
	if err != nil {
		return false, err
	}
	if sourceChanged && len(proofs) == 0 {
		gap = true
	}
	if len(proofs) > 1024 {
		gap = true
	}
	byID := make(map[string]domain.GuardianRequest, len(proofs))
	byOffset := make(map[int64]domain.GuardianRequest, len(proofs))
	for _, r := range proofs {
		byID[r.ResponseID] = r
		if !r.Conflict && r.SourceGeneration == b.Cursor.Generation && r.SourceOffset >= start && r.SourceOffset < offset {
			byOffset[r.SourceOffset] = r
		}
	}
	rewritten := false
	for _, r := range entries {
		if expected, ok := byOffset[r.offset]; ok {
			if !r.valid || !sameRequestMetadata(expected, r.request) {
				rewritten = true
			}
			delete(byOffset, r.offset)
		}
		if r.valid {
			expected, ok := byID[r.request.ResponseID]
			if !ok || !expected.Conflict && !sameRequestMetadata(expected, r.request) {
				gap = true
			}
		}
	}
	// A proof now inside another line also demonstrates that the prefix changed.
	rewritten = rewritten || len(byOffset) > 0
	if gap {
		b.Anomalies[domain.GuardianCoverageGap]++
	}
	if rewritten {
		return true, nil
	}
	b.Requests = append(b.Requests, probe.Requests...)
	b.Models = append(b.Models, probe.Models...)
	for code, count := range probe.Anomalies {
		b.Anomalies[code] += count
	}
	return false, nil
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
