package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// InsertExecutionUsage commits provenance and canonical usage together. Failed
// writes are replayable by the executor's pending receipt; no native row exists.
func (s *Store) InsertExecutionUsage(ctx context.Context, v domain.ExecutionUsageReceipt, digest string) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.qw.WithTx(tx)
	old, err := q.GetExecutionReceiptDigest(ctx, gen.GetExecutionReceiptDigestParams{SessionID: string(v.SessionID), BillingProvider: v.BillingProvider, ProfileID: v.ProfileID, ProfileRevision: v.ProfileRevision, DelegationID: v.DelegationID, AttemptID: v.AttemptID})
	if err == nil {
		if old != digest {
			return domain.ErrUsageSourceEventConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	identity := domain.ExecutionUsageIdentity{RequestedServiceTier: v.RequestedServiceTier, ObservedServiceTier: v.ObservedServiceTier, ServiceTierStatus: v.ServiceTierStatus, ProjectID: v.ProjectID, SessionID: v.SessionID, DelegationID: v.DelegationID, AttemptID: v.AttemptID, BillingProvider: v.BillingProvider, ProfileID: v.ProfileID, ProfileRevision: v.ProfileRevision, ProfileDigest: v.ProfileDigest, ProjectPolicyRevision: v.ProjectPolicyRevision, RequestID: v.RequestID, ResponseID: v.ResponseID, UnknownReason: v.UnknownReason, FinishReason: v.FinishReason}
	b, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	source, err := q.InsertExecutionSource(ctx, gen.InsertExecutionSourceParams{ProjectID: v.ProjectID, SessionID: string(v.SessionID), BillingProvider: v.BillingProvider, ProfileID: v.ProfileID, ProfileRevision: v.ProfileRevision, ProfileDigest: v.ProfileDigest, DelegationID: v.DelegationID, AttemptID: v.AttemptID, RequestID: v.RequestID, ReceiptDigest: digest, IdentityJson: string(b)})
	if err != nil {
		return fmt.Errorf("record execution source: %w", err)
	}
	// Only numeric fields pass through the existing provider-usage projection.
	usage := map[string]int64{}
	for name, value := range map[string]*int64{"cache_write_input_tokens": v.CacheWriteInputTokens, "input_tokens": v.Tokens.InputTokens, "cached_input_tokens": v.Tokens.CachedInputTokens, "output_tokens": v.Tokens.OutputTokens, "total_tokens": v.TotalTokens, "reasoning_output_tokens": v.ReasoningOutputTokens} {
		if value != nil {
			usage[name] = *value
		}
	}
	var projection sql.NullString
	if len(usage) > 0 {
		b, err = json.Marshal(map[string]any{"last_token_usage": usage})
		if err != nil {
			return err
		}
		projection = sql.NullString{String: string(b), Valid: true}
	}
	err = q.InsertExecutionUsageEvent(ctx, gen.InsertExecutionUsageEventParams{ExecutionSourceID: sql.NullInt64{Int64: source, Valid: true}, ProviderID: v.BillingProvider, ModelID: v.ModelID,
		InputTokens: ptrInt64ToNull(v.Tokens.InputTokens), CachedInputTokens: ptrInt64ToNull(v.Tokens.CachedInputTokens), UncachedInputTokens: ptrInt64ToNull(v.Tokens.UncachedInputTokens), OutputTokens: ptrInt64ToNull(v.Tokens.OutputTokens), ProviderUsageJson: projection, SourceEventKey: "execution:" + strconv.FormatInt(source, 10) + ":" + v.RequestID, CreatedAt: sql.NullTime{Time: v.ObservedAt, Valid: true}, InputCostNanos: ptrInt64ToNull(v.Costs.InputCostNanos), CachedInputCostNanos: ptrInt64ToNull(v.Costs.CachedInputCostNanos), OutputCostNanos: ptrInt64ToNull(v.Costs.OutputCostNanos), EstimatedCostNanos: ptrInt64ToNull(v.Costs.EstimatedCostNanos), PricingVersion: v.Costs.PricingVersion})
	if err != nil {
		return err
	}
	return tx.Commit()
}
