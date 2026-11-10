package rca

import (
	"regexp"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/components/hcg/policy"
)

var aoReference = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,256}$`)

func safeStoredSession(row storedSession) bool {
	if !aoReference.MatchString(string(row.ProjectID)) || !aoReference.MatchString(string(row.SessionID)) {
		return false
	}
	if row.Manifest.Revision > 0 && row.Manifest.Policy != policy.Resolve("worker", row.Manifest.Inputs) {
		return false
	}
	r := row.Recorded
	if r == nil {
		return true
	}
	if r.ProjectID != row.ProjectID || r.SessionID != row.SessionID || r.ObservedAt.IsZero() || !safeStatus(r.Status) || !safeReason(r.Reason) {
		return false
	}
	if r.ScopeRevision != "" && !validNativeID(r.ScopeRevision) {
		return false
	}
	for _, snap := range r.Snapshots {
		if !validNativeID(snap.ScopeID) || snap.Method != "utf8_bytes_div4_v1" || (snap.SourceVersion != "" && snap.SourceVersion != sourceVersion) || !safeStatus(snap.Status) {
			return false
		}
		if snap.Association != "registered_source" && snap.Association != "metadata_filename" {
			return false
		}
		for _, reason := range snap.Reasons {
			if !safeReason(reason) {
				return false
			}
		}
		if !safeBuckets(snap.Buckets) || !safeBuckets(snap.Unpaired) {
			return false
		}
		for _, turn := range snap.Turns {
			if turn.Ordinal <= 0 || !safeBuckets(turn.Buckets) {
				return false
			}
		}
	}
	return true
}
func safeBuckets(buckets [6]domain.RCABucket) bool {
	for i, b := range buckets {
		if b.Category != emptyBuckets()[i].Category || (b.Bytes == nil) != (b.EstimatedTokens == nil) {
			return false
		}
		if b.Bytes != nil && (*b.Bytes < 0 || *b.EstimatedTokens < 0) {
			return false
		}
	}
	return true
}
func safeStatus(status string) bool {
	return status == "ready" || status == "partial" || status == "unavailable"
}
func safeReason(reason string) bool {
	switch reason {
	case "", "invalid_snapshot", "window_head_gap", "source_read_failed", "canceled", "incomplete_tail", "record_limit", "parse_limit", "invalid_record", "source_identity_mismatch", "unsupported_source_version", "subagent_source", "fork_context_unknown", "projection_limit", "compaction_gap", "unknown_schema", "retained_context_unknown", "source_version_unknown", "hidden_thinking", "source_truncated", "missing_key", "unknown_fields", "unsupported_content", "no_observed_content", "source_missing", "ambiguous_source", "source_metadata_failed", "scope_root_mismatch", "scope_not_allowed", "native_generation_unknown", "native_identity_unknown", "source_enumeration_incomplete", "source_identity_changed", "policy_changed", "storage_unavailable", "storage_low_space", "storage_space_unknown", "storage_identity_changed", "storage_read_failed", "storage_write_failed", "session_limit", "host_limit", "disabled", "analysis_running", "analysis_not_requested":
		return true
	}
	return false
}
