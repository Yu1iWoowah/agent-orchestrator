package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func replacementHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateReplacementEvidence(op domain.OrchestratorReplacement) error {
	invalid := domain.ErrReplacementStateInvalid
	if p := op.Preparation; p != nil && p.PromptHash != "" && (!replacementHash(p.PromptHash) || p.PromptIntentAt == nil) {
		return invalid
	}
	if op.SourceStopIntentAt != nil && (op.FrozenAt == nil || op.Transcript == nil || op.SourceStopIntentAt.IsZero()) {
		return invalid
	}
	if op.FrozenAt != nil && (op.Transcript == nil || len(op.Transcript.FrozenHandoff) == 0 || len(op.Transcript.FrozenHandoff) > domain.MaxReplacementHandoffBytes || op.Preparation == nil || frozenHandoffHash(op) != op.Preparation.SubmittedHash) {
		return invalid
	}
	if stop := op.SourceStop; stop != nil {
		native, method := op.SourceOwner.AgentSessionID, "tui-fenced-stop"
		if op.SourceOwner.Mode == domain.SessionModeChat {
			native, method = op.SourceOwner.ProviderConversationID, "chat-hibernate"
		}
		if op.FrozenAt == nil || op.SourceStopIntentAt == nil || op.Transcript == nil || stop.Owner != op.SourceOwner || stop.CapabilityVerifier == "" || stop.CapabilityVerifier != op.SourceCapabilityVerifier || native == "" || stop.NativeID != native || stop.Method != method || stop.ConfirmedAt.IsZero() {
			return invalid
		}
	}
	t := op.Transcript
	if t == nil {
		return nil
	}
	if t.ExportedAt != nil && t.ExportedAt.IsZero() {
		return invalid
	}
	if t.CapturedAt == nil {
		if len(t.NativeCutoffs) != 0 || t.SnapshotFingerprint != "" || t.Receipt != nil || t.TranscriptHash != "" {
			return invalid
		}
		return nil
	}
	if t.CapturedAt.IsZero() || t.ExportedAt == nil || !replacementHash(t.SnapshotFingerprint) || len(t.NativeCutoffs) == 0 || len(t.NativeCutoffs) > 64 {
		return invalid
	}
	seen := map[string]bool{}
	var total int64
	for _, cut := range t.NativeCutoffs {
		if !replacementHash(cut.SHA256) || cut.NativeID == "" || cut.Schema == "" || cut.LeafID == "" || seen[cut.NativeID] || cut.Bytes <= 0 || cut.Bytes > (64<<20)-total {
			return invalid
		}
		seen[cut.NativeID] = true
		total += cut.Bytes
	}
	if t.Receipt != nil {
		r := t.Receipt
		if !filepath.IsAbs(r.Directory) || !replacementHash(r.ManifestHash) || !replacementHash(r.InputHash) || !replacementHash(t.TranscriptHash) {
			return invalid
		}
	} else if t.TranscriptHash != "" {
		return invalid
	}
	return nil
}

func replacementTimesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func preserveReplacementEvidence(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	invalid := fmt.Errorf("%w: captured evidence facts are immutable", domain.ErrReplacementStateInvalid)
	if before.FrozenAt == nil && after.FrozenAt != nil && ctx.Value(replacementFreezeKey{}) != q {
		return invalid
	}
	if after.Transcript != nil && after.Transcript.Receipt != nil && (before.Transcript == nil || before.Transcript.Receipt == nil) && ctx.Value(handoffEvidencePublicationKey{}) != q {
		return invalid
	}
	if p := before.Preparation; p != nil && p.PromptHash != "" {
		next := after.Preparation
		if next == nil || p.PromptHash != next.PromptHash || p.AttemptID != next.AttemptID || p.Nonce != next.Nonce || p.ControlClientID != next.ControlClientID || !replacementTimesEqual(p.PromptIntentAt, next.PromptIntentAt) || p.ControlTurnID != "" && p.ControlTurnID != next.ControlTurnID {
			return invalid
		}
	}
	if before.SourceStopIntentAt != nil && !replacementTimesEqual(before.SourceStopIntentAt, after.SourceStopIntentAt) {
		if ctx.Value(replacementUnattemptedStopKey{}) != q || after.SourceStopIntentAt != nil || before.SourceStop != nil || after.SourceStop != nil {
			return invalid
		}
	}
	if before.FrozenAt != nil && !replacementTimesEqual(before.FrozenAt, after.FrozenAt) {
		return invalid
	}
	if stop := before.SourceStop; stop != nil {
		next := after.SourceStop
		if next == nil || stop.Owner != next.Owner || stop.CapabilityVerifier != next.CapabilityVerifier || stop.NativeID != next.NativeID || stop.Method != next.Method || !stop.ConfirmedAt.Equal(next.ConfirmedAt) {
			return invalid
		}
	}
	t, next := before.Transcript, after.Transcript
	if t == nil {
		return nil
	}
	if next == nil {
		return invalid
	}
	if !bytes.Equal(t.FrozenHandoff, next.FrozenHandoff) {
		return invalid
	}
	if t.ExportedAt != nil && !replacementTimesEqual(t.ExportedAt, next.ExportedAt) {
		return invalid
	}
	if t.CapturedAt != nil && (!replacementTimesEqual(t.CapturedAt, next.CapturedAt) || t.SnapshotFingerprint != next.SnapshotFingerprint || !slices.Equal(t.NativeCutoffs, next.NativeCutoffs)) {
		return invalid
	}
	if t.Receipt != nil && (next.Receipt == nil || *t.Receipt != *next.Receipt || t.TranscriptHash != next.TranscriptHash) {
		return invalid
	}
	return nil
}
