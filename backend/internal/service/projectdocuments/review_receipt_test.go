package projectdocuments_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func TestGenericStateSaveCannotMintManagedWriteReceipt(t *testing.T) {
	for _, which := range []string{"first receipt", "replace real receipt"} {
		t.Run(which, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			op := replacementIntent(t, f)
			op, err := f.service.BeginReplacementPreparation(ctx, op.ID, op.Revision)
			if err != nil {
				t.Fatal(err)
			}
			grant, err := f.service.Acquire(ctx, "demo", f.actor)
			if err != nil {
				t.Fatal(err)
			}
			if which == "replace real receipt" {
				if _, err := f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: op.DocumentPath, Content: "real managed bytes", ExpectedHash: docs.MissingHash}); err != nil {
					t.Fatal(err)
				}
			}
			state, err := f.db.GetProjectDocumentState(ctx, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if state.LastHandoffWrite == nil {
				state.LastHandoffWrite = &domain.ProjectDocumentWriteReceipt{Path: op.DocumentPath, Hash: strings.Repeat("a", 64), PreviousHash: docs.MissingHash, SourceSessionID: f.owner.ID, Owner: f.owner.ControllerOwner(), CapabilityVerifier: f.owner.Metadata.BrowserCapabilityVerifier, LeaseEpoch: grant.Epoch, CreatedAt: time.Now()}
			}
			state.LastHandoffWrite.ID = "forged-without-a-managed-write"
			if saved, err := f.db.SaveProjectDocumentState(ctx, "demo", state); saved || err == nil {
				t.Fatalf("generic save minted %s: saved=%t err=%v", which, saved, err)
			}
		})
	}
}
