package projectdocuments_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func TestAcquirePredecessorLease(t *testing.T) {
	for _, mode := range []string{"terminated", "live", "frozen-terminated", "missing", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			old, err := f.service.Acquire(ctx, "demo", f.actor)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "frozen-terminated" {
				if err := f.service.Freeze(ctx, "demo", f.actor, old); err != nil {
					t.Fatal(err)
				}
			}
			before, err := f.db.GetProjectDocumentState(ctx, "demo")
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "terminated", "frozen-terminated":
				f.owner.IsTerminated = true
			case "live":
				// The caller is the only orchestrator, but the lease owner is still live.
				f.owner.Kind = domain.KindWorker
			}
			if mode != "missing" {
				if err := f.db.UpdateSession(ctx, f.owner); err != nil {
					t.Fatal(err)
				}
			}
			successor := f.owner
			successor.ID = ""
			successor.IsTerminated = false
			successor.Kind = domain.KindOrchestrator
			successor.Metadata.ControllerGeneration = "successor"
			successor, err = f.db.CreateSession(ctx, successor)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				deleted, err := f.db.DeleteSession(ctx, f.owner.ID)
				if err != nil || !deleted {
					t.Fatalf("delete seed owner: deleted=%v err=%v", deleted, err)
				}
			}
			actor := docs.Actor{SessionID: successor.ID, Capability: "actor-secret"}
			next, err := f.service.Acquire(ctx, "demo", actor)
			if mode != "terminated" {
				code := "PROJECT_DOCUMENT_LEASE_HELD"
				switch mode {
				case "frozen-terminated":
					code = "PROJECT_DOCUMENT_LEASE_FROZEN"
				case "ambiguous":
					code = "PROJECT_DOCUMENT_OWNER_AMBIGUOUS"
				}
				var ae *apierr.Error
				if !errors.As(err, &ae) || ae.Code != code {
					t.Fatalf("Acquire error=%v, want %s", err, code)
				}
				if code == "PROJECT_DOCUMENT_LEASE_HELD" && !strings.Contains(ae.Message, string(f.owner.ID)) {
					t.Fatalf("held error omits owner %s: %s", f.owner.ID, ae.Message)
				}
				after, err := f.db.GetProjectDocumentState(ctx, "demo")
				if err != nil || after.Revision != before.Revision || *after.Lease != *before.Lease {
					t.Fatalf("rejected acquisition changed lease: before=%+v after=%+v err=%v", before, after, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("terminated owner's lease was not reclaimed: %v", err)
			}
			if next.Epoch != old.Epoch+1 || next.Token == "" || next.Token == old.Token {
				t.Fatalf("reclaimed grant=%+v; expected next epoch and new token", next)
			}
			info, err := f.service.Info(ctx, "demo")
			if err != nil || info.Lease.SessionID != successor.ID || info.Lease.Epoch != next.Epoch {
				t.Fatalf("reclaimed lease=%+v err=%v", info.Lease, err)
			}
			// Use the authorized successor with the old grant to exercise lease fencing,
			// rather than merely rejecting the terminated actor at authorization.
			write := docs.WriteInput{Actor: actor, Lease: old, Path: "handoff/reclaimed.md", Content: "successor", ExpectedHash: docs.MissingHash}
			_, err = f.service.Write(ctx, "demo", write)
			var ae *apierr.Error
			if !errors.As(err, &ae) || ae.Code != "PROJECT_DOCUMENT_LEASE_INVALID" {
				t.Fatalf("old grant error=%v, want LEASE_INVALID", err)
			}
			write.Lease = next
			if _, err := f.service.Write(ctx, "demo", write); err != nil {
				t.Fatalf("successor cannot write: %v", err)
			}
			// A restarted service must recover the same durable grant.
			restarted := docs.New(docs.Deps{Store: f.db, DataDir: f.root, Capabilities: capability{}})
			repeated, err := restarted.Acquire(ctx, "demo", actor)
			if err != nil || repeated != next {
				t.Fatalf("repeated grant=%+v err=%v, want same grant", repeated, err)
			}
		})
	}
}
