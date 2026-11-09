package usage

import (
	"context"
	"errors"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"strings"
	"testing"
	"time"
)

type executionSink struct {
	receipt domain.ExecutionUsageReceipt
	digest  string
	calls   int
	err     error
}

func (s *executionSink) InsertExecutionUsage(_ context.Context, r domain.ExecutionUsageReceipt, digest string) error {
	s.receipt = r
	s.digest = digest
	s.calls++
	return s.err
}

func TestExecutionRecorderUnknownAndReplayFailure(t *testing.T) {
	sink := &executionSink{err: errors.New("write unavailable")}
	recorder := NewExecutionRecorder(sink)
	receipt := domain.ExecutionUsageReceipt{ProjectID: "p", SessionID: "s", DelegationID: "d", AttemptID: "a", BillingProvider: "anthropic", ProfileID: "fixture", ProfileRevision: "r1", ProfileDigest: strings.Repeat("a", 64), ProjectPolicyRevision: "policy-r1", ModelID: "fixture-model", RequestID: "known-request", UnknownReason: "usage_missing", ObservedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}
	if err := recorder.RecordExecutionUsage(context.Background(), receipt); !errors.Is(err, sink.err) {
		t.Fatalf("lost write failure: %v", err)
	}
	digest := sink.digest
	sink.err = nil
	if err := recorder.RecordExecutionUsage(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	if sink.digest != digest || sink.receipt.ProfileRevision != "r1" || sink.receipt.ProjectPolicyRevision != "policy-r1" || sink.receipt.RequestID != "known-request" || sink.receipt.Tokens.InputTokens != nil {
		t.Fatalf("pending receipt changed: %+v", sink.receipt)
	}
	zero := int64(0)
	receipt.Tokens.InputTokens = &zero
	if err := recorder.RecordExecutionUsage(context.Background(), receipt); err == nil || sink.calls != 2 {
		t.Fatal("UNKNOWN accepted invented zero")
	}
}
