package projectdocuments_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func replacementEvidence(t *testing.T, f fixture, confirmed bool) (domain.OrchestratorReplacement, transcriptexport.Output) {
	t.Helper()
	ctx := context.Background()
	op := replacementIntent(t, f)
	op, err := f.service.BeginReplacementPreparation(ctx, op.ID, op.Revision)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := f.service.Acquire(ctx, op.ProjectID, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.service.Write(ctx, op.ProjectID, docs.WriteInput{Actor: f.actor, Lease: grant, Path: op.DocumentPath, Content: "# operation-authored handoff", ExpectedHash: docs.MissingHash})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op.Preparation.SubmittedHash, op.Preparation.SubmittedPath, op.Preparation.WriteReceiptID = file.Hash, file.Path, file.WriteReceiptID
	op.Preparation.VerifiedAt, op.Preparation.SubmittedAt = &now, &now
	op.Preparation.ControlTurnID = "prepared-control"
	op.Phase = domain.ReplacementHandoffVerified
	if ok, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
		t.Fatal(ok, err)
	}
	op.Revision++
	op.Phase = domain.ReplacementFreezingSharedWrites
	if saved, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal(saved, err)
	}

	op, _, err = f.db.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	op, err = f.service.FreezeReplacementHandoff(ctx, op.ID, op.Revision)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := f.db.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: f.owner.ID, Harness: domain.HarnessClaudeCode, NativeRootID: "native-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.InsertUsageSource(ctx, domain.UsageSourceRecord{BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain, NativeSessionID: "native-1", ArtifactPath: "/fixture/projects/workspace/native-1.jsonl", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	source, _, err := f.db.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.db.SetSessionHibernated(ctx, source.ID, source.Revision, &now); err != nil || !ok {
		t.Fatal(ok, err)
	}
	snapshot, err := f.db.LoadTranscriptSourceSnapshot(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	cuts := []transcriptexport.EpochCutoff{{SHA256: strings.Repeat("a", 64), NativeID: "native-1", Schema: ports.ClaudeTranscriptSchemaV1, Bytes: 10, LeafID: "u"}}
	out, err := transcriptexport.Build(ctx, snapshot, cuts, evidenceReader{})
	if err != nil {
		t.Fatal(err)
	}
	op.Transcript.ExportedAt, op.Transcript.CapturedAt = &now, &now
	op.Transcript.NativeCutoffs, op.Transcript.SnapshotFingerprint = cuts, snapshot.Fingerprint
	op.SourceStopIntentAt = &now
	if confirmed {
		op.SourceStop = &domain.ReplacementControllerStop{Owner: source.ControllerOwner(), CapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, NativeID: "native-1", Method: "chat-hibernate", ConfirmedAt: now}
	}
	if confirmed {
		if ok, err := f.db.SaveReplacementRuntimeProgress(ctx, op); err != nil || !ok {
			t.Fatal(ok, err)
		}
	} else {
		// Deliberately seed an unconfirmed stop for the document negative test.
		op = sqlitetest.SeedReplacement(t, f.dbDir, op)
	}
	op, _, err = f.db.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return op, out
}

func TestReplacementEvidencePublicationBindsReceiptWithoutRawCapability(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, out := replacementEvidence(t, f, true)
	result, err := f.service.PublishReplacementEvidence(ctx, op.ID, op.Revision, out)
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != domain.ReplacementTranscriptVerified || result.Transcript.Receipt == nil || result.Transcript.TranscriptHash == "" {
		t.Fatal("publication facts not durable", result.Phase)
	}
	receipt := result.Transcript.Receipt
	if receipt.Directory != filepath.Join(f.root, "projects", "demo", "orchestrator", "handoffs", op.ID, op.Transcript.AttemptID) {
		t.Fatal("wrong evidence location", receipt.Directory)
	}
	file, err := f.service.ReadEvidence(ctx, op.ProjectID, op.ID, op.Transcript.AttemptID, "transcript.md")
	if err != nil || file.Hash != result.Transcript.TranscriptHash || !strings.Contains(file.Content, "preserved source") {
		t.Fatal("immutable transcript/hash unavailable", err)
	}
	saved, found, err := f.db.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || !found || saved.Transcript.Receipt == nil || *saved.Transcript.Receipt != *receipt {
		t.Fatal("operation receipt differs", err)
	}
	source, _, err := f.db.GetSession(ctx, op.SourceSessionID)
	if err != nil || source.IsTerminated {
		t.Fatal("publication retired source", err)
	}
	encoded, err := json.Marshal(saved)
	if err != nil || strings.Contains(string(encoded), f.actor.Capability) {
		t.Fatal("raw capability persisted", err)
	}
}

type failedReplacementEvidenceStore struct {
	*sqlite.Store
	fail bool
}

func (s *failedReplacementEvidenceStore) SaveReplacementEvidence(ctx context.Context, op domain.OrchestratorReplacement) (bool, error) {
	if s.fail && op.Phase == domain.ReplacementTranscriptVerified {
		return false, syscall.ENOSPC
	}
	return s.Store.SaveReplacementEvidence(ctx, op)
}

func TestReplacementEvidencePublicationRecoversBundleAfterFinalSaveFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, out := replacementEvidence(t, f, true)
	store := &failedReplacementEvidenceStore{Store: f.db, fail: true}
	service := docs.New(docs.Deps{Store: store, DataDir: f.root, Capabilities: capability{}})
	if _, err := service.PublishReplacementEvidence(ctx, op.ID, op.Revision, out); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("missing injected final-save failure: %v", err)
	}
	before, _, err := f.db.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || before.Phase != domain.ReplacementExportingTranscript || before.Transcript.Receipt != nil {
		t.Fatal("failed transaction leaked operation publication", err)
	}
	state, err := f.db.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || len(state.Evidence) != 0 {
		t.Fatal("failed transaction leaked receipt", err)
	}
	store.fail = false
	after, err := service.PublishReplacementEvidence(ctx, op.ID, op.Revision, out)
	if err != nil || after.Transcript.Receipt == nil || after.Phase != domain.ReplacementTranscriptVerified {
		t.Fatal("same-attempt bundle not recovered", err)
	}
}

func TestReplacementEvidencePublicationRequiresConfirmedSourceStop(t *testing.T) {
	f := setup(t)
	op, out := replacementEvidence(t, f, false)
	if _, err := f.service.PublishReplacementEvidence(context.Background(), op.ID, op.Revision, out); err == nil {
		t.Fatal("hibernation marker alone authorized export publication")
	}
}

type changedReplacementEvidenceSource struct {
	*sqlite.Store
	before func()
}

func (s *changedReplacementEvidenceSource) WithReplacementDocumentPublication(ctx context.Context, id string, revision int64, source domain.SessionRecord, documentRevision int64, publish func(context.Context) error) (bool, error) {
	if s.before != nil {
		s.before()
		s.before = nil
	}
	return s.Store.WithReplacementDocumentPublication(ctx, id, revision, source, documentRevision, publish)
}

func TestReplacementEvidenceRechecksQuiescenceInsidePublicationReservation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, out := replacementEvidence(t, f, true)
	store := &changedReplacementEvidenceSource{Store: f.db, before: func() {
		source, _, err := f.db.GetSession(ctx, op.SourceSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := f.db.SetSessionHibernated(ctx, source.ID, source.Revision, nil); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}}
	service := docs.New(docs.Deps{Store: store, DataDir: f.root, Capabilities: capability{}})
	if _, err := service.PublishReplacementEvidence(ctx, op.ID, op.Revision, out); err == nil {
		t.Fatal("publication accepted source after its quiescence marker was cleared")
	}
	state, err := f.db.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || len(state.Evidence) != 0 {
		t.Fatal("changed source published evidence", err)
	}
}
