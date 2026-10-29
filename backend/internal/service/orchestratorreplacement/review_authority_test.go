package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Regression for audit60's normally-admitted service counterexample.
func TestReplacementAdmittedOperationCannotMintSuccess(t *testing.T) {
	for _, phase := range []domain.ReplacementPhase{domain.ReplacementTranscriptVerified, domain.ReplacementCompleted, domain.ReplacementStarting, domain.ReplacementControllerReady, domain.ReplacementHandoffSettled, domain.ReplacementVerifyingResume, domain.ReplacementActivatingSharedWriter, domain.ReplacementNotifying} {
		t.Run(string(phase), func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			op, err := f.svc.Accept(ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = phase
			if phase == domain.ReplacementTranscriptVerified {
				now := time.Now().UTC()
				op.FrozenAt, op.SourceStopIntentAt = &now, &now
				native := op.SourceOwner.ProviderConversationID
				op.SourceStop = &domain.ReplacementControllerStop{Owner: op.SourceOwner, CapabilityVerifier: op.SourceCapabilityVerifier, NativeID: native, Method: "chat-hibernate", ConfirmedAt: now}
				op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}, CapturedAt: &now, ExportedAt: &now, SnapshotFingerprint: op.RequestHash, NativeCutoffs: []domain.TranscriptEpochCutoff{{SHA256: op.RequestHash, NativeID: native, Schema: "fixture/1", Bytes: 10, LeafID: "leaf"}}, Receipt: &domain.HandoffEvidenceReceipt{Directory: t.TempDir(), ManifestHash: op.RequestHash, InputHash: op.RequestHash}, TranscriptHash: op.RequestHash}
			}
			if ok, err := f.store.SaveOrchestratorReplacement(ctx, op); ok || err == nil {
				t.Fatalf("generic save minted unverified success phase=%s saved=%t err=%v", phase, ok, err)
			}
			stored, err := f.svc.Get(ctx, op.ID)
			if err != nil || stored.Phase != domain.ReplacementAccepted {
				t.Fatal("rejected write changed service-readable facts", stored.Phase, err)
			}
		})
	}
}

func TestReplacementAdmissionRejectsMissingSourceIdentity(t *testing.T) {
	for _, field := range []string{"verifier", "native"} {
		t.Run(field, func(t *testing.T) {
			f := setup(t)
			if field == "verifier" {
				f.source.Metadata.BrowserCapabilityVerifier = ""
			} else {
				f.source.Metadata.ProviderConversationID = ""
			}
			if err := f.store.UpdateSession(context.Background(), f.source); err != nil {
				t.Fatal(err)
			}
			_, err := f.svc.Accept(context.Background(), f.request)
			code(t, err, "ORCHESTRATOR_HANDOFF_SOURCE_UNVERIFIED")
			if f.runtime.preflights != 0 || f.docs.calls != 0 {
				t.Fatal("unidentified source reached side effects")
			}
		})
	}
}

func TestReplacementListIsolatesMalformedOperation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, err := f.svc.Accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec(`INSERT INTO orchestrator_replacements (id,project_id,source_session_id,operation_key,request_hash,phase,is_active,revision,intake_revision,state_json,created_at,updated_at)
SELECT '44444444-4444-4444-8444-444444444444','malformed-project',source_session_id,'55555555-5555-4555-8555-555555555555','bad',phase,is_active,1,0,
json_set(state_json,'$.id','44444444-4444-4444-8444-444444444444','$.projectId','malformed-project','$.operationKey','55555555-5555-4555-8555-555555555555','$.requestHash','bad'),created_at,updated_at
FROM orchestrator_replacements WHERE id=?`, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	active, err := f.store.ListActiveOrchestratorReplacements(ctx)
	if err != nil || len(active) != 1 || active[0].ID != op.ID {
		t.Fatalf("one malformed row stopped healthy reconcile: ops=%v err=%v", active, err)
	}
}
