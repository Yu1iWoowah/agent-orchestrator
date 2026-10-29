package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementInitialCreateCannotMintEvidence(t *testing.T) {
	for _, which := range []string{"evidence", "retirement"} {
		t.Run(which, func(t *testing.T) {
			st := newTestStore(t)
			op := replacementOperation(t, st)
			op.Phase = domain.ReplacementRejected
			now := time.Now().UTC()
			if which == "retirement" {
				op.SourceRetiredAt = &now
			} else {
				body := []byte("not a managed freeze")
				sum := sha256.Sum256(body)
				op.Preparation = &domain.ReplacementPreparation{VerifiedAt: &now, SubmittedHash: hex.EncodeToString(sum[:])}
				op.FrozenAt = &now
				op.Transcript = &domain.ReplacementTranscript{FrozenHandoff: body, AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}, CapturedAt: &now, ExportedAt: &now, SnapshotFingerprint: strings.Repeat("a", 64), NativeCutoffs: []domain.TranscriptEpochCutoff{{SHA256: strings.Repeat("b", 64), NativeID: "native-source", Schema: "fixture/1", Bytes: 10, LeafID: "leaf"}}, Receipt: &domain.HandoffEvidenceReceipt{Directory: t.TempDir(), ManifestHash: strings.Repeat("c", 64), InputHash: strings.Repeat("d", 64)}, TranscriptHash: strings.Repeat("e", 64)}
			}
			if _, created, err := st.CreateOrchestratorReplacement(context.Background(), op); created || err == nil {
				t.Fatalf("initial rejected create minted %s: created=%t err=%v", which, created, err)
			}
			if _, found, err := st.GetOrchestratorReplacement(context.Background(), op.ID); found || err != nil {
				t.Fatal("unverified initial facts persisted", found, err)
			}
		})
	}
}
