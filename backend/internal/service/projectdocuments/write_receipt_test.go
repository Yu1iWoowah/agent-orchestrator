package projectdocuments_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestHandoffWriteHasDurableAuthorReceipt(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	in := docs.WriteInput{Actor: f.actor, Lease: grant, Path: "handoff/orchestrator-handoff.md", Content: "# operation-specific handoff", ExpectedHash: docs.MissingHash}
	file, err := f.service.Write(ctx, "demo", in)
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	r := state.LastHandoffWrite
	if r == nil || r.ID == "" || file.WriteReceiptID != r.ID || r.Hash != file.Hash || r.Path != in.Path || r.PreviousHash != docs.MissingHash || r.Owner != f.owner.ControllerOwner() || r.SourceSessionID != f.owner.ID || r.LeaseEpoch != grant.Epoch || r.CapabilityVerifier != f.owner.Metadata.BrowserCapabilityVerifier || r.CreatedAt.IsZero() {
		t.Fatalf("missing author/hash/epoch receipt: %+v", r)
	}
	reopened, err := sqlite.OpenPreMigrated(f.dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	durable, err := reopened.GetProjectDocumentState(ctx, "demo")
	if err != nil || durable.LastHandoffWrite == nil || durable.LastHandoffWrite.ID != r.ID {
		t.Fatal("receipt did not survive reopen", err)
	}
	// Reads do not mint receipts. Failed CAS and unrelated writes cannot claim
	// to have authored a new handoff.
	read, err := f.service.Read(ctx, "demo", in.Path)
	if err != nil || read.WriteReceiptID != "" {
		t.Fatal("read minted write receipt", err)
	}
	if _, err := f.service.Write(ctx, "demo", in); err == nil {
		t.Fatal("stale write accepted")
	}
	in.Path = "rules/rule.md"
	other, err := f.service.Write(ctx, "demo", in)
	if err != nil || other.WriteReceiptID != "" {
		t.Fatal("unrelated file minted handoff receipt", err)
	}
	after, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil || after.LastHandoffWrite == nil || after.LastHandoffWrite.ID != r.ID {
		t.Fatal("failed/unrelated write changed handoff receipt", err)
	}
}

type receiptFailStore struct {
	*sqlite.Store
	fail bool
}

func (s *receiptFailStore) SaveProjectDocumentState(ctx context.Context, id domain.ProjectID, state domain.ProjectDocumentState) (bool, error) {
	if s.fail && state.LastHandoffWrite != nil {
		return false, errors.New("injected receipt save failure")
	}
	return s.Store.SaveProjectDocumentState(ctx, id, state)
}
func TestHandoffWriteDoesNotClaimSuccessAfterReceiptFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	st := &receiptFailStore{Store: f.db, fail: true}
	svc := docs.New(docs.Deps{Store: st, DataDir: f.root, Capabilities: capability{}})
	file, err := svc.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: "handoff/orchestrator-handoff.md", Content: "# unreceipted", ExpectedHash: docs.MissingHash})
	if err == nil || file.WriteReceiptID != "" {
		t.Fatal("unreceipted publication reported success")
	}
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil || state.LastHandoffWrite != nil {
		t.Fatal("failed publication left a successful author receipt", err)
	}
}
