package projectdocuments_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func replacementIntent(t *testing.T, f fixture) domain.OrchestratorReplacement {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	info, err := f.service.Info(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := f.db.CreateOrchestratorReplacement(ctx, domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64), ProjectID: "demo", SourceSessionID: f.owner.ID, SourceOwner: f.owner.ControllerOwner(), SourceCapabilityVerifier: f.owner.Metadata.BrowserCapabilityVerifier, TargetAccountID: "target-account", SharedDir: info.SharedDir, DocumentPath: info.DocumentPath, Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []domain.ReplacementPhase{domain.ReplacementPreflight, domain.ReplacementWaitingSourceIdle} {
		op.Phase = phase
		if ok, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
			t.Fatal(ok, err)
		}
		op.Revision++
	}
	op.Phase = domain.ReplacementAdoptingSharedFiles
	op.Preparation = &domain.ReplacementPreparation{AttemptID: "33333333-3333-4333-8333-333333333333", Nonce: strings.Repeat("b", 64), ControlClientID: "replacement/" + op.ID + "/prepare/1", StartedAt: now, DeadlineAt: now.Add(10 * time.Minute), ExpectedSessions: []domain.SessionID{}}
	if ok, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
		t.Fatal(ok, err)
	}
	op, _, err = f.db.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestReplacementHandoffFreezeRequiresCurrentVerifiedAuthorReceipt(t *testing.T) {
	for _, which := range []string{"valid", "receipt mismatch", "file changed", "unverified", "source rotated", "late input", "new worker"} {
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
			file, err := f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: op.DocumentPath, Content: "verified operation-specific contents", ExpectedHash: docs.MissingHash})
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = domain.ReplacementHandoffVerified
			op.Preparation.SubmittedHash, op.Preparation.SubmittedPath, op.Preparation.WriteReceiptID = file.Hash, file.Path, file.WriteReceiptID
			now := time.Now().UTC()
			op.Preparation.VerifiedAt, op.Preparation.SubmittedAt = &now, &now
			op.Preparation.ControlTurnID = "prepared-control"
			switch which {
			case "receipt mismatch":
				op.Preparation.WriteReceiptID = "another-write"
			case "file changed":
				if err := os.WriteFile(filepath.Join(op.SharedDir, filepath.FromSlash(op.DocumentPath)), []byte("changed after verification"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unverified":
				op.Preparation.VerifiedAt = nil
				op.Phase = domain.ReplacementPreparingHandoff
			case "source rotated":
				f.owner.Metadata.BrowserCapabilityVerifier = "another-verifier"
				if err := f.db.UpdateSession(ctx, f.owner); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if which != "unverified" {
				op.Revision++
				op.Phase = domain.ReplacementFreezingSharedWrites
				if saved, err := f.db.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
					t.Fatal(saved, err)
				}
			}

			op, _, err = f.db.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if which == "late input" {
				conv, err := f.db.CreateConversation(ctx, "late-project-intake", domain.ConversationScopeProject, op.ProjectID, op.SourceSessionID, now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.AppendUserMessage(ctx, conv.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "late-user", ClientMessageID: "late-user", Text: "new direction", Origin: domain.MessageOriginHuman}, "late-turn", now); err != nil {
					t.Fatal(err)
				}
			}
			if which == "new worker" {
				if _, err := f.db.CreateSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			frozen, err := f.service.FreezeReplacementHandoff(ctx, op.ID, op.Revision)
			state, readErr := f.db.GetProjectDocumentState(ctx, "demo")
			if readErr != nil {
				t.Fatal(readErr)
			}
			if which != "valid" {
				if err == nil || state.Lease.Frozen {
					t.Fatal("unverified/stale handoff frozen", err)
				}
				return
			}
			if err != nil || !state.Lease.Frozen || frozen.FrozenAt == nil || frozen.Phase != domain.ReplacementExportingTranscript {
				t.Fatalf("verified handoff not frozen: %+v %v", frozen, err)
			}
			if frozen.Transcript == nil || frozen.Transcript.AO.ConversationCutoffs == nil || frozen.Transcript.AttemptID == "" {
				t.Fatal("freeze omitted durable AO cutoff and evidence attempt")
			}
			if _, err := f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: op.DocumentPath, Content: "late write", ExpectedHash: file.Hash}); err == nil {
				t.Fatal("source wrote after snapshot boundary")
			}
		})
	}
}
func TestReplacementPreparationLeaseClaimIsBoundAndIdempotent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	old, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: old, Path: "handoff/orchestrator-handoff.md", Content: "old handoff", ExpectedHash: docs.MissingHash})
	if err != nil {
		t.Fatal(err)
	}
	op := replacementIntent(t, f)
	prepared, err := f.service.BeginReplacementPreparation(ctx, op.ID, op.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Preparation == nil || prepared.Preparation.LeaseEpoch != old.Epoch+1 || prepared.Preparation.BaselineHash != file.Hash || prepared.Preparation.BaselineWriteReceiptID != file.WriteReceiptID || prepared.Phase != domain.ReplacementPreparingHandoff {
		t.Fatalf("baseline/epoch not durable: %+v", prepared)
	}
	state, err := f.db.GetProjectDocumentState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if state.Lease.ReplacementID != op.ID || state.Lease.ClaimVerifier != f.owner.Metadata.BrowserCapabilityVerifier || state.Lease.TokenHash != "" {
		t.Fatal("pending claim was not owner/verifier-bound")
	}
	if _, err := f.service.Acquire(ctx, "demo", docs.Actor{SessionID: f.owner.ID, Capability: "forged"}); err == nil {
		t.Fatal("forged capability claimed source lease")
	}
	grant, err := f.service.Acquire(ctx, "demo", f.actor)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Epoch != prepared.Preparation.LeaseEpoch || grant.Token == "" {
		t.Fatal("source did not obtain prepared epoch")
	}
	if _, err := f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: old, Path: "handoff/orchestrator-handoff.md", Content: "stale writer", ExpectedHash: file.Hash}); err == nil {
		t.Fatal("old grant survived preparation fencing")
	}
	again, err := f.service.BeginReplacementPreparation(ctx, prepared.ID, prepared.Revision)
	if err != nil || again.Revision != prepared.Revision || again.Preparation.LeaseEpoch != grant.Epoch {
		t.Fatal("same attempt reset its baseline/epoch", err)
	}
	if _, err := f.service.Write(ctx, "demo", docs.WriteInput{Actor: f.actor, Lease: grant, Path: "handoff/orchestrator-handoff.md", Content: "current attempt", ExpectedHash: file.Hash}); err != nil {
		t.Fatal("claimed writer cannot prepare document", err)
	}
}
