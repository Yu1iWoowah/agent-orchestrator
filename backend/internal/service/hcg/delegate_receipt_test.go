package hcg

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

type canonicalReceiptCapture struct{ receipt domain.ExecutionUsageReceipt }

func (c *canonicalReceiptCapture) RecordExecutionUsage(_ context.Context, r domain.ExecutionUsageReceipt) error {
	c.receipt = r
	return nil
}
func TestDelegationReceiptProjectionPreservesDispatchIdentity(t *testing.T) {
	sink := &canonicalReceiptCapture{}
	recorder := delegationReceiptRecorder{canonical: sink}
	input, output, total, cached, cost := int64(10), int64(2), int64(12), int64(3), int64(100)
	receipt := reporting.DelegationUsageReceipt{ProjectID: "p", SessionID: "s", DelegationID: "d", AttemptID: "a", ProjectPolicyRevision: "old-policy", BillingProvider: "openai", ProfileID: "fixture", ProfileRevision: "v1", ProfileDigest: "old-descriptor", Model: "fixture-model", RequestID: "request", ObservedAt: time.Unix(0, 0), InputTokens: &input, CachedInputTokens: &cached, OutputTokens: &output, TotalTokens: &total, Costs: reporting.DelegationCharges{TotalNanos: &cost, PriceVersion: "old-price"}}
	if err := recorder.RecordDelegationUsage(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	got := sink.receipt
	if got.ProjectID != "p" || got.SessionID != "s" || got.ProfileRevision != "v1" || got.ProjectPolicyRevision != "old-policy" || got.Costs.PricingVersion != "old-price" || got.Tokens.UncachedInputTokens == nil || *got.Tokens.UncachedInputTokens != 7 || *got.Costs.EstimatedCostNanos != 100 {
		t.Fatalf("receipt=%+v", got)
	}
	receipt.BillingProvider = "anthropic"
	receipt.CachedInputTokens = nil
	receipt.InputTokens = nil
	receipt.OutputTokens = nil
	receipt.TotalTokens = nil
	receipt.UnknownReason = "usage_missing"
	receipt.Costs = reporting.DelegationCharges{}
	if err := recorder.RecordDelegationUsage(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	if sink.receipt.Tokens.InputTokens != nil || sink.receipt.Tokens.UncachedInputTokens != nil || sink.receipt.Tokens.CachedInputTokens != nil || sink.receipt.UnknownReason != "usage_missing" {
		t.Fatalf("UNKNOWN filled: %+v", sink.receipt)
	}
}
