package projectdocuments_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func TestHandoffPublishedWriteFailureNamesUnconfirmedReceipt(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	fault := &receiptFailStore{Store: f.db, fail: true}
	svc := docs.New(docs.Deps{Store: fault, DataDir: f.root, Capabilities: capability{}})
	in := docs.WriteInput{Actor: f.actor, Lease: grant, Path: "handoff/orchestrator-handoff.md", Content: "# authored but not receipted", ExpectedHash: docs.MissingHash}
	result, err := svc.Write(ctx, "demo", in)
	var named *apierr.Error
	if !errors.As(err, &named) || named.Code != "PROJECT_DOCUMENT_WRITE_RECEIPT_UNCONFIRMED" || result.WriteReceiptID != "" {
		t.Fatalf("published write failed without safe recovery classification: file=%+v err=%v", result, err)
	}
	read, err := svc.Read(ctx, "demo", in.Path)
	if err != nil || read.Content != in.Content {
		t.Fatal("failure was not after publication", read, err)
	}
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil || state.LastHandoffWrite != nil {
		t.Fatal("failed publication minted receipt", state.LastHandoffWrite, err)
	}
}

func TestHandoffReceiptFailureRecoversOnlyWithReReadHashAndManagedWrite(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	fault := &receiptFailStore{Store: f.db, fail: true}
	svc := docs.New(docs.Deps{Store: fault, DataDir: f.root, Capabilities: capability{}})
	in := docs.WriteInput{Actor: f.actor, Lease: grant, Path: "handoff/orchestrator-handoff.md", Content: "# recoverable managed contents", ExpectedHash: docs.MissingHash}
	if file, err := svc.Write(ctx, "demo", in); err == nil || file.WriteReceiptID != "" {
		t.Fatal("receipt failure reported success", file, err)
	}
	fault.fail = false
	if _, err := svc.Write(ctx, "demo", in); err == nil {
		t.Fatal("stale expected hash silently retried publication")
	}
	current, err := svc.Read(ctx, "demo", in.Path)
	if err != nil {
		t.Fatal(err)
	}
	in.ExpectedHash = current.Hash
	recovered, err := svc.Write(ctx, "demo", in)
	if err != nil || recovered.WriteReceiptID == "" || recovered.Hash != current.Hash {
		t.Fatal("explicit CAS retry could not create managed receipt", recovered, err)
	}
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil || state.LastHandoffWrite == nil || state.LastHandoffWrite.ID != recovered.WriteReceiptID || state.LastHandoffWrite.Hash != recovered.Hash || state.LastHandoffWrite.SourceSessionID != f.actor.SessionID || state.LastHandoffWrite.LeaseEpoch != grant.Epoch {
		t.Fatal("recovery receipt not bound to current managed author", state.LastHandoffWrite, err)
	}
}
