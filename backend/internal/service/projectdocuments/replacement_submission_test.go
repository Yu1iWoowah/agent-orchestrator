package projectdocuments_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func TestReplacementPreparationSubmissionBindsBytesAuthorAndAttempt(t *testing.T) {
	for _, which := range []string{"valid", "nonce", "hash", "receipt", "actor", "format", "rotated source"} {
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
			file, err := f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: op.DocumentPath, Content: "this attempt's handoff", ExpectedHash: docs.MissingHash})
			if err != nil {
				t.Fatal(err)
			}
			in := docs.ReplacementPreparationInput{Actor: f.actor, AttemptID: op.Preparation.AttemptID, Nonce: op.Preparation.Nonce, Path: op.DocumentPath, Hash: file.Hash, WriteReceiptID: file.WriteReceiptID}
			format := func(actual docs.File) error {
				if actual.Content != file.Content {
					return errors.New("wrong bytes validated")
				}
				return nil
			}
			switch which {
			case "nonce":
				in.Nonce = "old-nonce"
			case "hash":
				in.Hash = "another-hash"
			case "receipt":
				in.WriteReceiptID = "another-receipt"
			case "actor":
				in.Actor.Capability = "forged"
			case "format":
				format = func(docs.File) error { return errors.New("missing section B") }
			case "rotated source":
				f.owner.Metadata.BrowserCapabilityVerifier = "rotated"
				if err := f.db.UpdateSession(ctx, f.owner); err != nil {
					t.Fatal(err)
				}
			}
			submitted, err := f.service.RecordReplacementPreparation(ctx, op.ID, op.Revision, in, format)
			got, _, readErr := f.db.GetOrchestratorReplacement(ctx, op.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if which != "valid" {
				if err == nil || got.Preparation.SubmittedAt != nil {
					t.Fatal("invalid preparation submission persisted", err)
				}
				return
			}
			if err != nil || submitted.Preparation.SubmittedAt == nil || submitted.Preparation.SubmittedHash != file.Hash || submitted.Preparation.WriteReceiptID != file.WriteReceiptID || submitted.Preparation.VerifiedAt != nil || submitted.Phase != domain.ReplacementPreparingHandoff {
				t.Fatalf("submission skipped turn settlement: %+v %v", submitted, err)
			}
			again, err := f.service.RecordReplacementPreparation(ctx, submitted.ID, submitted.Revision, in, format)
			if err != nil || again.Revision != submitted.Revision {
				t.Fatal("same receipt was not idempotent", err)
			}
		})
	}
}
