package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type executionStore interface {
	InsertExecutionUsage(context.Context, domain.ExecutionUsageReceipt, string) error
}

// ExecutionRecorder validates trusted receipts before the canonical transaction.
type ExecutionRecorder struct{ store executionStore }

var _ ports.ExecutionUsageRecorder = (*ExecutionRecorder)(nil)

// NewExecutionRecorder constructs the daemon-only receipt port.
func NewExecutionRecorder(s executionStore) *ExecutionRecorder { return &ExecutionRecorder{store: s} }

// RecordExecutionUsage never resolves a newer policy, profile or price version.
func (r *ExecutionRecorder) RecordExecutionUsage(ctx context.Context, v domain.ExecutionUsageReceipt) error {
	for _, id := range []string{v.ProjectID, string(v.SessionID), v.DelegationID, v.AttemptID, v.ProfileID, v.ProfileRevision, v.ProjectPolicyRevision, v.ModelID} {
		if strings.TrimSpace(id) == "" || len(id) > 256 {
			return fmt.Errorf("invalid execution identity")
		}
	}
	digest, err := hex.DecodeString(v.ProfileDigest)
	if err != nil || len(digest) != 32 || v.ObservedAt.IsZero() || (v.BillingProvider != "openai" && v.BillingProvider != "anthropic") {
		return fmt.Errorf("invalid execution descriptor")
	}
	if len(v.RequestID) > 256 || len(v.ResponseID) > 256 || len(v.UnknownReason) > 128 || len(v.FinishReason) > 128 {
		return fmt.Errorf("invalid execution projection")
	}
	for _, n := range []*int64{v.Tokens.InputTokens, v.Tokens.CachedInputTokens, v.Tokens.UncachedInputTokens, v.Tokens.OutputTokens, v.TotalTokens, v.ReasoningOutputTokens, v.Costs.InputCostNanos, v.Costs.CachedInputCostNanos, v.Costs.OutputCostNanos, v.Costs.EstimatedCostNanos} {
		if n != nil && *n < 0 {
			return fmt.Errorf("negative execution counter")
		}
	}
	if v.UnknownReason != "" {
		if v.Tokens.InputTokens != nil || v.Tokens.OutputTokens != nil || v.Tokens.CachedInputTokens != nil || v.Tokens.UncachedInputTokens != nil || v.TotalTokens != nil || v.ReasoningOutputTokens != nil || v.Costs.EstimatedCostNanos != nil || v.Costs.InputCostNanos != nil || v.Costs.CachedInputCostNanos != nil || v.Costs.OutputCostNanos != nil {
			return fmt.Errorf("unknown execution usage must not invent counters")
		}
	} else if v.RequestID == "" || v.Tokens.InputTokens == nil || v.Tokens.OutputTokens == nil {
		return fmt.Errorf("known execution usage requires request identity and counters")
	}
	if i, c, u := v.Tokens.InputTokens, v.Tokens.CachedInputTokens, v.Tokens.UncachedInputTokens; i != nil && c != nil && u != nil && (*c > math.MaxInt64-*u || *i != *c+*u) {
		return fmt.Errorf("inconsistent execution input")
	}
	if i, o, n := v.Tokens.InputTokens, v.Tokens.OutputTokens, v.TotalTokens; i != nil && o != nil && n != nil && (*i > math.MaxInt64-*o || *n != *i+*o) {
		return fmt.Errorf("inconsistent execution total")
	}
	if c, i := v.Tokens.CachedInputTokens, v.Tokens.InputTokens; c != nil && (i == nil || *c > *i) {
		return fmt.Errorf("invalid cached input")
	}
	if n, o := v.ReasoningOutputTokens, v.Tokens.OutputTokens; n != nil && (o == nil || *n > *o) {
		return fmt.Errorf("invalid reasoning output")
	}
	if v.Costs.EstimatedCostNanos != nil && strings.TrimSpace(v.Costs.PricingVersion) == "" {
		return fmt.Errorf("execution price revision required")
	}
	v.ObservedAt = v.ObservedAt.UTC()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	return r.store.InsertExecutionUsage(ctx, v, hex.EncodeToString(sum[:]))
}
