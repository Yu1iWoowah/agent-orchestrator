package sqlitetest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// SeedReplacement preloads trusted prior-stage facts in an isolated test DB.
// It bypasses managed publication deliberately; tests of admission, evidence or
// receipt production must use real producers instead. No runtime imports this
// test utility package. The normal decoder validates the seeded row on read.
func SeedReplacement(t testing.TB, dir string, op domain.OrchestratorReplacement) domain.OrchestratorReplacement {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	q := gen.New(db)
	data, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	n, err := q.UpdateOrchestratorReplacement(ctx, gen.UpdateOrchestratorReplacementParams{ID: op.ID, Revision: op.Revision, Phase: string(op.Phase), IsActive: !op.Phase.Terminal(), StateJson: string(data), UpdatedAt: op.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")})
	if err != nil || n != 1 {
		t.Fatal("seed replacement CAS", n, err)
	}
	row, err := q.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	err = q.InsertReplacementRepairSnapshot(ctx, gen.InsertReplacementRepairSnapshotParams{OperationID: op.ID, Revision: row.Revision, Digest: hex.EncodeToString(sum[:]), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	op.Revision = row.Revision
	return op
}

// SeedProjectDocuments is reserved for low-level CAS/fault fixtures. Managed
// write/publication tests must never use it as evidence that a receipt exists.
func SeedProjectDocuments(t testing.TB, dir string, id domain.ProjectID, state domain.ProjectDocumentState) domain.ProjectDocumentState {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO project_document_states(project_id,revision,state_json) VALUES (?,1,?) ON CONFLICT(project_id) DO UPDATE SET revision=revision+1,state_json=excluded.state_json`, string(id), string(data))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// ReplacementPriorFacts supplies prior-stage facts only for isolated lifecycle fixtures.
func ReplacementPriorFacts(op domain.OrchestratorReplacement) domain.OrchestratorReplacement {
	now := op.UpdatedAt
	switch op.Phase {
	case domain.ReplacementPreparingHandoff, domain.ReplacementHandoffVerified, domain.ReplacementFreezingSharedWrites:
		if op.Preparation == nil {
			op.Preparation = &domain.ReplacementPreparation{LeaseEpoch: 7}
		}
	}
	later := op.Phase == domain.ReplacementStarting || op.Phase == domain.ReplacementNotifying || op.Phase == domain.ReplacementCompleted
	if op.FrozenAt != nil || later {
		if op.FrozenAt == nil {
			op.FrozenAt = &now
		}
		if op.Transcript == nil {
			op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}}
		}
		if len(op.Transcript.FrozenHandoff) == 0 {
			op.Transcript.FrozenHandoff = []byte("verified fixture handoff")
		}
		sum := sha256.Sum256(op.Transcript.FrozenHandoff)
		if op.Preparation == nil {
			op.Preparation = &domain.ReplacementPreparation{}
		}
		op.Preparation.SubmittedHash = hex.EncodeToString(sum[:])
		op.Preparation.LeaseEpoch = 7
		op.Preparation.VerifiedAt = &now
	}
	if later {
		op.SourceStopIntentAt = &now
		native, method := op.SourceOwner.ProviderConversationID, "chat-hibernate"
		if op.SourceOwner.Mode != domain.SessionModeChat {
			native, method = op.SourceOwner.AgentSessionID, "tui-fenced-stop"
		}
		op.SourceStop = &domain.ReplacementControllerStop{Owner: op.SourceOwner, CapabilityVerifier: op.SourceCapabilityVerifier, NativeID: native, Method: method, ConfirmedAt: now}
		op.SourceRetiredAt = &now
		tr := op.Transcript
		tr.CapturedAt, tr.ExportedAt = &now, &now
		tr.SnapshotFingerprint = strings.Repeat("a", 64)
		tr.NativeCutoffs = []domain.TranscriptEpochCutoff{{NativeID: native, SHA256: strings.Repeat("b", 64), Schema: "fixture/1", Bytes: 10, LeafID: "leaf"}}
		tr.Receipt = &domain.HandoffEvidenceReceipt{Directory: "/fixture/evidence", ManifestHash: strings.Repeat("c", 64), InputHash: strings.Repeat("d", 64)}
		tr.TranscriptHash = strings.Repeat("e", 64)
	}
	if op.Phase == domain.ReplacementNotifying || op.Phase == domain.ReplacementCompleted {
		if op.SuccessorSessionID == "" {
			op.SuccessorSessionID = "fixture-successor"
		}
		owner := op.SourceOwner
		owner.Harness = domain.HarnessClaudeCode
		owner.Mode = domain.SessionModeChat
		owner.ControllerGeneration = "successor-before"
		owner.ProviderConversationID = "native-successor"
		op.SuccessorController = &domain.ReplacementSuccessorController{Owner: owner, CapabilityVerifier: "successor-verifier", CommittedAt: now}
		op.BootstrapClientID = "fixture-bootstrap"
		op.Bootstrap = &domain.ReplacementBootstrap{PromptHash: strings.Repeat("f", 64), IntentAt: now, TurnID: "bootstrap-turn", SettledAt: &now}
		after := owner
		after.ControllerGeneration = "successor-after"
		op.ResumeProbeClientID = "fixture-probe"
		op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: owner, BeforeNativeID: owner.ProviderConversationID, BeforeCapabilityVerifier: "successor-verifier", AfterOwner: after, AfterNativeID: owner.ProviderConversationID, AfterCapabilityVerifier: "resumed-verifier", StopConfirmed: true, HistoryVerified: true, StopReceipt: &domain.ReplacementControllerStop{Owner: owner, NativeID: owner.ProviderConversationID, CapabilityVerifier: "successor-verifier", Method: "chat-hibernate", ConfirmedAt: now}, ResumeIntentAt: &now, ResumeGeneration: after.ControllerGeneration, ResumedAt: &now, VerifiedAt: &now, Probe: &domain.ReplacementContinuityProbe{Nonce: "44444444-4444-4444-8444-444444444444", PromptHash: strings.Repeat("a", 64), IntentAt: now, TurnID: "probe-turn", SettledAt: &now}}
		op.WriterActivation = &domain.ReplacementWriterActivation{BeforeEpoch: 7, AfterEpoch: 8, Owner: after, CapabilityVerifier: "resumed-verifier", ActivatedAt: now}
		if op.Phase == domain.ReplacementCompleted {
			op.IntakeReleasedAt, op.IntakeDrainRequestedAt = &now, &now
		}
	}

	return op
}

// FillReplacementMetadataHistory creates a quota-state fixture in one local
// transaction. It changes metadata only, serializes every capsule and digest,
// and leaves all immutable triggers enabled. Boundary tests must still use the
// real producer to verify refusal/recovery after this setup. A dedicated store
// test covers ordinary managed saves; repeating that setup per phase adds only
// fsync/decode overhead, not another boundary assertion.
func FillReplacementMetadataHistory(t testing.TB, dir, id string, limit int64) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	q := gen.New(tx)
	ctx := context.Background()
	row, err := q.GetOrchestratorReplacement(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var op domain.OrchestratorReplacement
	if err := json.Unmarshal([]byte(row.StateJson), &op); err != nil {
		t.Fatal(err)
	}
	originalRevision := row.Revision
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO orchestrator_replacement_repair_snapshots(operation_id,revision,digest,payload) VALUES (?,?,?,?)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stmt.Close() }()
	for row.Revision < limit {
		op.Error = &domain.ReplacementFailure{Code: "QUOTA_FIXTURE", Message: fmt.Sprint(row.Revision)}
		data, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		row.Revision++
		row.StateJson = string(data)
		payload, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		if _, err := stmt.ExecContext(ctx, id, row.Revision, hex.EncodeToString(sum[:]), payload); err != nil {
			t.Fatal(err)
		}
	}
	result, err := tx.ExecContext(ctx, "UPDATE orchestrator_replacements SET state_json=?,revision=? WHERE id=? AND revision=?", row.StateJson, row.Revision, id, originalRevision)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatal("quota fixture final CAS", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
