package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func replacementRetirementFixture(t *testing.T, dir, changed string) (*sqlite.Store, domain.OrchestratorReplacement, domain.SessionRecord, domain.ProjectDocumentState) {
	t.Helper()
	ctx := context.Background()
	st := sqlitetest.MustOpenAt(t, dir)
	initial := replacementOperation(t, st)
	project, _, err := st.GetProject(ctx, string(initial.ProjectID))
	if err != nil {
		t.Fatal(err)
	}
	initial.ProjectFingerprint, err = domain.ReplacementProjectFingerprint(project)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := st.CreateOrchestratorReplacement(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if ok, err := st.SetSessionHibernated(ctx, source.ID, source.Revision, &now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	source, _, err = st.GetSession(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase, op.FrozenAt, op.SourceStopIntentAt = domain.ReplacementRetiring, &now, &now
	op.Preparation = &domain.ReplacementPreparation{LeaseEpoch: 7, VerifiedAt: &now, WriteReceiptID: "write-receipt", SubmittedHash: strings.Repeat("a", 64)}
	op.SourceStop = &domain.ReplacementControllerStop{Owner: op.SourceOwner, CapabilityVerifier: op.SourceCapabilityVerifier, NativeID: op.SourceOwner.ProviderConversationID, Method: "chat-hibernate", ConfirmedAt: now}
	op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}, CapturedAt: &now, ExportedAt: &now, SnapshotFingerprint: strings.Repeat("b", 64), NativeCutoffs: []domain.TranscriptEpochCutoff{{NativeID: "native-source", LeafID: "assistant", Schema: "2.1.0", Bytes: 7, SHA256: strings.Repeat("c", 64)}}, TranscriptHash: strings.Repeat("d", 64), Receipt: &domain.HandoffEvidenceReceipt{Directory: filepath.Join(dir, "evidence"), ManifestHash: strings.Repeat("e", 64), InputHash: strings.Repeat("f", 64)}}
	state := domain.ProjectDocumentState{Lease: &domain.ProjectDocumentLease{ReplacementID: op.ID, SessionID: source.ID, Owner: op.SourceOwner, Epoch: 7, Frozen: true, TokenHash: strings.Repeat("1", 64)}, Evidence: map[string]domain.HandoffEvidenceReceipt{op.ID + "/" + op.Transcript.AttemptID: *op.Transcript.Receipt}}
	state.LastHandoffWrite = &domain.ProjectDocumentWriteReceipt{ID: op.Preparation.WriteReceiptID, Path: op.DocumentPath, Hash: op.Preparation.SubmittedHash, SourceSessionID: source.ID, Owner: op.SourceOwner, CapabilityVerifier: op.SourceCapabilityVerifier, LeaseEpoch: 7, CreatedAt: now}
	switch changed {
	case "phase":
		op.Phase = domain.ReplacementTranscriptVerified
	case "missing stop":
		op.SourceStop = nil
	case "missing receipt":
		op.Transcript.Receipt = nil
		op.Transcript.TranscriptHash = ""
	case "thawed lease":
		state.Lease.Frozen = false
	}
	if changed == "missing stop" || changed == "missing receipt" {
		op.Phase = domain.ReplacementExportingTranscript
	}
	op = sqlitetest.ReplacementPriorFacts(op)
	op = sqlitetest.SeedReplacement(t, dir, op)
	state.LastHandoffWrite.Hash = op.Preparation.SubmittedHash
	sqlitetest.SeedProjectDocuments(t, dir, op.ProjectID, state)
	state, err = st.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	return st, op, source, state
}

func TestReplacementRetirementCommitsSourceAndOperationAtomically(t *testing.T) {
	for _, changed := range []string{"current", "phase", "missing stop", "missing receipt", "thawed lease", "stale revision", "rotated capability", "guard failure", "missing guard"} {
		t.Run(changed, func(t *testing.T) {
			ctx := context.Background()
			st, op, source, state := replacementRetirementFixture(t, t.TempDir(), changed)
			worker, err := st.CreateSession(ctx, sampleRecord("replace"))
			if err != nil {
				t.Fatal(err)
			}
			worker, _, err = st.GetSession(ctx, worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			revision := op.Revision
			if changed == "stale revision" {
				revision--
			}
			if changed == "rotated capability" {
				rec := source
				rec.Metadata.BrowserCapabilityVerifier = "rotated"
				if err := st.UpdateSession(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			checks := 0
			guard := func(context.Context) error {
				checks++
				if changed == "guard failure" {
					return errors.New("native witness changed")
				}
				return nil
			}
			if changed == "missing guard" {
				guard = nil
			}
			next, applied, err := st.CommitReplacementRetirement(ctx, op.ID, revision, source, state.Revision, guard)
			current, _, readErr := st.GetSession(ctx, source.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			stored, _, readErr := st.GetOrchestratorReplacement(ctx, op.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if changed == "current" {
				if err != nil || !applied || !current.IsTerminated || next.SourceRetiredAt == nil || stored.SourceRetiredAt == nil || stored.Phase != domain.ReplacementStarting || checks != 1 {
					t.Fatalf("retirement not atomic: applied=%t terminated=%t phase=%s retired=%v checks=%d err=%v", applied, current.IsTerminated, stored.Phase, stored.SourceRetiredAt, checks, err)
				}
				if current.Metadata.WorkspacePath != source.Metadata.WorkspacePath || current.Metadata.Branch != source.Metadata.Branch || current.Metadata.ProviderConversationID != source.Metadata.ProviderConversationID {
					t.Fatal("retirement discarded source restore/history facts")
				}
			} else if applied || current.IsTerminated || stored.SourceRetiredAt != nil {
				t.Fatalf("%s retired unverified source: applied=%t terminated=%t retired=%v err=%v", changed, applied, current.IsTerminated, stored.SourceRetiredAt, err)
			}
			after, _, err := st.GetSession(ctx, worker.ID)
			if err != nil || !reflect.DeepEqual(after, worker) {
				t.Fatal("retirement changed worker", err)
			}
		})
	}
}

func TestReplacementRetirementFinalSaveFailureRollsBackAndRetryUsesSameIntent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, op, source, state := replacementRetirementFixture(t, dir, "current")
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER reject_retirement_completion BEFORE UPDATE ON orchestrator_replacements WHEN NEW.phase='starting' BEGIN SELECT RAISE(ABORT,'completion ENOSPC'); END`); err != nil {
		t.Fatal(err)
	}
	guard := func(context.Context) error { return nil }
	if _, applied, err := st.CommitReplacementRetirement(ctx, op.ID, op.Revision, source, state.Revision, guard); err == nil || applied {
		t.Fatalf("completion fault not surfaced: applied=%t err=%v", applied, err)
	}
	current, _, err := st.GetSession(ctx, source.ID)
	if err != nil || current.IsTerminated {
		t.Fatal("failed operation save left source retired", err)
	}
	if _, err := raw.ExecContext(ctx, "DROP TRIGGER reject_retirement_completion"); err != nil {
		t.Fatal(err)
	}
	next, applied, err := st.CommitReplacementRetirement(ctx, op.ID, op.Revision, source, state.Revision, guard)
	if err != nil || !applied || next.ID != op.ID || next.Phase != domain.ReplacementStarting {
		t.Fatalf("same intent did not recover: applied=%t phase=%s err=%v", applied, next.Phase, err)
	}
}
