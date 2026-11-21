package reporting

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/delegate"
)

// DelegationUsageReceipt is the fixed executor's safe daemon projection. It is
// not a self-report/import request and carries no prompt/source/credential body.
type DelegationUsageReceipt struct {
	CacheWriteInputTokens                                                            *int64 `json:"cacheWriteInputTokens,omitempty"`
	RequestedServiceTier                                                             string `json:"requestedServiceTier,omitempty"`
	ObservedServiceTier                                                              string `json:"observedServiceTier,omitempty"`
	ServiceTierStatus                                                                string `json:"serviceTierStatus,omitempty"`
	ProjectID, SessionID, DelegationID, AttemptID                                    string
	ProjectPolicyRevision                                                            string
	BillingProvider, ProfileID, ProfileRevision, ProfileDigest                       string
	Model, RequestID, ResponseID, UnknownReason, FinishReason                        string
	ObservedAt                                                                       time.Time
	InputTokens, CachedInputTokens, OutputTokens, TotalTokens, ReasoningOutputTokens *int64
	Costs                                                                            DelegationCharges
}
type DelegationCharges struct {
	InputNanos, CachedInputNanos, OutputNanos, TotalNanos *int64
	PriceVersion                                          string
}
type DelegationUsageRecorder interface {
	RecordDelegationUsage(context.Context, DelegationUsageReceipt) error
}

type delegationReceiptAdapter struct{ sink DelegationUsageRecorder }

func (a delegationReceiptAdapter) Record(ctx context.Context, r delegate.Receipt) error {
	if a.sink == nil {
		return delegate.ErrUnavailable
	}
	return a.sink.RecordDelegationUsage(ctx, DelegationUsageReceipt{
		RequestedServiceTier: r.RequestedServiceTier, ObservedServiceTier: r.ObservedServiceTier, ServiceTierStatus: r.ServiceTierStatus, CacheWriteInputTokens: r.Usage.CacheWriteInput,
		ProjectID: r.Execution.ProjectID, SessionID: r.Execution.SessionID, DelegationID: r.Execution.DelegationID, AttemptID: r.Execution.AttemptID, ProjectPolicyRevision: r.Execution.ProjectPolicyRevision,
		BillingProvider: r.Provider, ProfileID: r.ProfileID, ProfileRevision: r.ProfileRevision, ProfileDigest: r.ProfileDigest, Model: r.Model, RequestID: r.RequestID, ResponseID: r.ResponseID, UnknownReason: r.UnknownReason, FinishReason: r.FinishReason, ObservedAt: r.ObservedAt,
		InputTokens: r.Usage.Input, CachedInputTokens: r.Usage.CachedInput, OutputTokens: r.Usage.Output, TotalTokens: r.Usage.Total, ReasoningOutputTokens: r.Usage.ReasoningOutput,
		Costs: DelegationCharges{InputNanos: r.Charges.Input, CachedInputNanos: r.Charges.CachedInput, OutputNanos: r.Charges.Output, TotalNanos: r.Charges.Total, PriceVersion: r.Charges.PriceVersion}})
}
