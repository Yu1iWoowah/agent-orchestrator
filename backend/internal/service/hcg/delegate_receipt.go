package hcg

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

// Only the trusted executor receives this adapter; no HTTP request supplies a
// token vector or another project's identity. The canonical transaction checks
// session ownership and receipt conflicts, including late dispatch revisions.
type delegationReceiptRecorder struct{ canonical ports.ExecutionUsageRecorder }

var _ reporting.DelegationUsageRecorder = delegationReceiptRecorder{}

func (r delegationReceiptRecorder) RecordDelegationUsage(ctx context.Context, v reporting.DelegationUsageReceipt) error {
	tokens := domain.UsageTokenMetrics{InputTokens: v.InputTokens, CachedInputTokens: v.CachedInputTokens, OutputTokens: v.OutputTokens}
	if v.InputTokens != nil && v.CachedInputTokens != nil && *v.CachedInputTokens >= 0 && *v.CachedInputTokens <= *v.InputTokens {
		uncached := *v.InputTokens - *v.CachedInputTokens
		tokens.UncachedInputTokens = &uncached
	}
	return r.canonical.RecordExecutionUsage(ctx, domain.ExecutionUsageReceipt{
		RequestedServiceTier: v.RequestedServiceTier, ObservedServiceTier: v.ObservedServiceTier, ServiceTierStatus: v.ServiceTierStatus, CacheWriteInputTokens: v.CacheWriteInputTokens,
		ProjectID: v.ProjectID, SessionID: domain.SessionID(v.SessionID), DelegationID: v.DelegationID, AttemptID: v.AttemptID, ProjectPolicyRevision: v.ProjectPolicyRevision,
		BillingProvider: v.BillingProvider, ProfileID: v.ProfileID, ProfileRevision: v.ProfileRevision, ProfileDigest: v.ProfileDigest, ModelID: v.Model, RequestID: v.RequestID, ResponseID: v.ResponseID, UnknownReason: v.UnknownReason, FinishReason: v.FinishReason, ObservedAt: v.ObservedAt, Tokens: tokens, TotalTokens: v.TotalTokens, ReasoningOutputTokens: v.ReasoningOutputTokens,
		Costs: domain.UsageEventCosts{InputCostNanos: v.Costs.InputNanos, CachedInputCostNanos: v.Costs.CachedInputNanos, OutputCostNanos: v.Costs.OutputNanos, EstimatedCostNanos: v.Costs.TotalNanos, PricingVersion: v.Costs.PriceVersion}})
}
