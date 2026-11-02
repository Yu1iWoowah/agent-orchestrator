package domain

import (
	"errors"
	"slices"
	"strings"
)

// ErrInvalidChatJournal rejects unbounded or unclassified diagnostic material.
var ErrInvalidChatJournal = errors.New("invalid chat journal record")

func journalID(s string) bool {
	return s != "" && len(s) <= 256 && !strings.ContainsFunc(s, func(r rune) bool {
		return r != '-' && r != '_' && r != '.' && r != ':' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
}

func journalOwner(owner AgentSettingsOwner) bool {
	return journalID(string(owner.SessionID)) && journalID(owner.ControllerGeneration) && journalID(owner.AccountID) && journalID(owner.BranchID) && journalID(owner.ProviderConversationID)
}

func journalDeliveryOwner(owner AgentSettingsOwner) bool {
	if !journalID(string(owner.SessionID)) || !journalID(owner.ControllerGeneration) || !journalID(owner.BranchID) {
		return false
	}
	for _, id := range []string{owner.AccountID, owner.ProviderConversationID} {
		if id != "" && !journalID(id) {
			return false
		}
	}
	return true
}

func journalCode(s string) bool {
	return len(s) <= 96 && !strings.ContainsFunc(s, func(r rune) bool { return r != '_' && (r < 'A' || r > 'Z') && (r < '0' || r > '9') })
}

func journalRequestID(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 256 || strings.Contains(s, ":/") {
		return false
	}
	parts := strings.Split(s, "/")
	if len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if !journalID(part) {
			return false
		}
	}
	return true
}

// Validate enforces the safe diagnostic vocabulary before persistence or DTO use.
func (r AgentSettingsReconciliation) Validate() error {
	if r.RetryOfVersion < 0 || r.RetryOfAttemptID != "" && !journalID(r.RetryOfAttemptID) {
		return ErrInvalidChatJournal
	}
	if !journalID(r.ID) || !journalOwner(r.Owner) || r.SessionID != r.Owner.SessionID || r.Revision < 0 || r.Version < 0 || r.RetryCount < 0 || r.RetryCount > 3 || !journalID(r.ExecutorID) || r.CreatedAt.IsZero() || r.LastProgressAt.Before(r.CreatedAt) || r.Deadline.Before(r.CreatedAt) || r.ClaimUntil.Before(r.CreatedAt) || r.Desired.Validate() != nil || r.PreferenceAtStart.Validate() != nil || !journalCode(r.ErrorCode) {
		return ErrInvalidChatJournal
	}
	for _, id := range []string{r.OperationID, r.ProviderEpoch, r.ActionKey, r.ObservationRef} {
		if id != "" && !journalID(id) {
			return ErrInvalidChatJournal
		}
	}
	if !journalRequestID(r.RequestID) {
		return ErrInvalidChatJournal
	}
	if !slices.Contains([]string{"", "unknown", "provider_current", "per_turn_wire"}, r.EvidenceKind) {
		return ErrInvalidChatJournal
	}
	if r.EffectPending && r.ActionKey == "" || r.Phase == "ready" && r.EffectPending {
		return ErrInvalidChatJournal
	}
	if r.OriginReplacementOperationID != nil && !journalID(*r.OriginReplacementOperationID) {
		return ErrInvalidChatJournal
	}
	if !slices.Contains([]string{"observing", "waiting_idle", "applying", "verifying", "ready", "blocked", "recovery_required", "cancelled"}, r.Phase) || !slices.Contains([]string{"unknown", "available", "unsupported"}, r.ObservationAvailability) {
		return ErrInvalidChatJournal
	}
	steps := []string{"", "observing", "preparing", "waiting_idle", "applying_model", "applying_effort", "applying_fast", "verifying", "verified", "committing", "rolling_back", "restored"}
	for _, step := range []string{r.Step, r.LastSuccessfulStep, r.FailedStep} {
		if !slices.Contains(steps, step) {
			return ErrInvalidChatJournal
		}
	}
	if !slices.Contains([]string{"", "conflict", "cancelled", "provider", "storage", "validation", "recovery_required"}, r.ErrorCategory) || !slices.Contains([]string{"", "provider_unclassified", "timeout", "cancelled", "capabilities_unavailable", "unsupported", "owner_changed", "storage_failed", "waiting_provider_work", "permission_unknown"}, r.SafeCause) || !slices.Contains([]string{"", "unknown", "unsupported", "authentication_required", "rate_limited", "busy", "invalid_request", "transport_unavailable"}, r.ProviderErrorCode) {
		return ErrInvalidChatJournal
	}
	for _, missing := range r.MissingEvidence {
		if !slices.Contains([]string{"current_settings", "provider_idle", "provider_epoch", "owner", "permission_policy"}, missing) {
			return ErrInvalidChatJournal
		}
	}
	for _, action := range r.RecoveryActions {
		if !slices.Contains([]string{"retry", "restore_permission", "rebind", "wait_for_provider", "wait_for_work", "repair_storage", "submit_new_settings"}, action) {
			return ErrInvalidChatJournal
		}
	}
	if r.HistoricalApplied != nil && r.HistoricalApplied.Validate() != nil || r.Observed != nil && r.Observed.Validate() != nil || r.ObservationAvailability != "available" && r.Observed != nil {
		return ErrInvalidChatJournal
	}
	return nil
}

// Validate rejects payloads and free-form provider text in delivery evidence.
func (a ChatDeliveryAttempt) Validate() error {
	if !journalID(a.ID) || !journalID(a.ConversationID) || !journalDeliveryOwner(a.Owner) || a.SessionID != a.Owner.SessionID || a.Revision < 0 || a.Version < 0 || len(a.TurnIDs) < 1 || len(a.TurnIDs) > 1000 || a.CreatedAt.IsZero() || a.UpdatedAt.Before(a.CreatedAt) || a.LastProgressAt.Before(a.CreatedAt) || !journalCode(a.ErrorCode) || a.AttentionID != "" && !journalID(a.AttentionID) {
		return ErrInvalidChatJournal
	}
	if a.PreparedTurnID != "" && !journalID(a.PreparedTurnID) {
		return ErrInvalidChatJournal
	}
	if len(a.InputRefs) > 0 {
		if len(a.InputRefs) != len(a.TurnIDs) {
			return ErrInvalidChatJournal
		}
		for i, ref := range a.InputRefs {
			if ref.TurnID != a.TurnIDs[i] || !journalID(ref.BranchID) || !journalID(string(ref.SessionID)) {
				return ErrInvalidChatJournal
			}
		}
	}
	seen := make(map[string]bool, len(a.TurnIDs))
	for _, id := range a.TurnIDs {
		if !journalID(id) || seen[id] {
			return ErrInvalidChatJournal
		}
		seen[id] = true
	}
	if !slices.Contains([]ChatDeliveryDisposition{ChatDeliveryReserved, ChatDeliveryNotSent, ChatDeliveryUnknown, ChatDeliveryAccepted}, a.Disposition) || a.EvidenceLevel < 0 || a.EvidenceLevel > 2 {
		return ErrInvalidChatJournal
	}
	if a.Receipt != nil {
		return a.Receipt.Validate()
	}
	return nil
}

// Validate checks vocabulary only; Accepts additionally checks correlation and
// monotonic delivery facts against the original attempt.
func (r ChatDeliveryReceipt) Validate() error {
	if r.ProgressSequence < 0 {
		return ErrInvalidChatJournal
	}
	if r.ExecutionSettled && r.Disposition != ChatDeliveryAccepted && r.Disposition != ChatDeliveryNotSent {
		return ErrInvalidChatJournal
	}
	if !journalID(r.AttemptID) || r.ObservedAt.IsZero() || !slices.Contains([]string{"provider_response", "provider_event", "not_forwarded", "local_not_forwarded", "incomplete"}, r.Source) || !slices.Contains([]string{"", "running", "approval", "tool", "background"}, r.Progress) {
		return ErrInvalidChatJournal
	}
	for _, id := range []string{r.ProviderTurnID, r.ProviderRequestID, r.ProviderEpoch} {
		if id != "" && !journalID(id) {
			return ErrInvalidChatJournal
		}
	}
	return nil
}
