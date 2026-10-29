package orchestratorreplacement_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func TestReplacementCancelDeletedSourceReleasesWriter(t *testing.T) {
	x := retryFrozenFixture(t)
	ctx := context.Background()
	before, err := x.f.store.GetProjectDocumentState(ctx, x.operation.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	raw := replacementRawDB(t, x)
	if _, err := raw.Exec(`DELETE FROM sessions WHERE id=?`, x.source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := x.service().Cancel(ctx, x.operation.ID); err != nil {
		t.Fatal("source deletion prevented cancellation", err)
	}
	replacement := x.source
	replacement.Metadata.ControllerGeneration = "new-sole-orchestrator"
	replacement.CreatedAt, replacement.UpdatedAt = time.Now(), time.Now()
	replacement, err = x.f.store.CreateSession(ctx, replacement)
	if err != nil {
		t.Fatal("cancel retained orchestrator reservation", err)
	}
	actor := projectdocuments.Actor{SessionID: replacement.ID, Capability: "author-secret"}
	grant, err := x.documents.Acquire(ctx, replacement.ProjectID, actor)
	if err != nil || grant.Epoch <= before.Lease.Epoch {
		t.Fatal("revoked missing owner still held the lease", grant.Epoch, err)
	}
	file, err := x.documents.Read(ctx, replacement.ProjectID, x.operation.DocumentPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.documents.Write(ctx, replacement.ProjectID, projectdocuments.WriteInput{Actor: actor, Lease: grant, Path: file.Path, Content: "current orchestrator after cancellation", ExpectedHash: file.Hash}); err != nil {
		t.Fatal("new sole orchestrator cannot write", err)
	}
	if *x.actualStops != 0 {
		t.Fatal("cancellation stopped another controller")
	}
}
