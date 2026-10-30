package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func activationCoordinatorFixture(t *testing.T) *preparedExport {
	return probeSettlementFixture(t, true)
}

func probeSettlementFixture(t *testing.T, settle bool) *preparedExport {
	t.Helper()
	x := resumedSuccessorCoordinatorFixture(t)
	ctx := context.Background()
	op := x.operation
	now := time.Now().UTC()
	conv, err := x.f.store.CreateProjectConversationAdoptingQueue(ctx, "activation-project", op.ProjectID, op.SuccessorSessionID, domain.ConversationActivity{}, now)
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		t.Fatal(err)
	}
	branch := domain.ConversationBranch{ID: "activation-native-root", ConversationID: conv.ID, SessionID: target.ID, ParentBranchID: conv.ActiveBranchID, ProviderScopeID: "activation-native-root", ProviderConversationID: target.Metadata.ProviderConversationID, Strategy: domain.ConversationBranchStrategyNative, ForkAfterSequence: conv.LatestSequence, CreatedAt: now}
	if err := x.f.store.CommitChatSpawn(ctx, target, branch); err != nil {
		t.Fatal(err)
	}
	x.f.runtime.probe = func(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (replacement.PreparationDelivery, error) {
		if !allowNew {
			t.Fatal("fixture unexpectedly tried lookup-only probe")
		}
		const turnID = "activation-probe-turn"
		if _, err := x.f.store.AppendUserMessage(ctx, conv.ID, target.ID, target.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "activation-probe-message", ClientMessageID: op.ResumeProbeClientID, Text: prompt, Origin: domain.MessageOriginAutomation}, turnID, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := x.f.store.BindTurnToProvider(ctx, turnID, "activation-provider-probe", time.Now()); err != nil {
			t.Fatal(err)
		}
		ack, ok := domain.ReplacementProbeAcknowledgment(op)
		if !ok {
			t.Fatal("missing fixture probe nonce")
		}
		if err := x.f.store.SettleAssistantMessage(ctx, conv.ID, "activation-probe-answer-item", "activation-provider-probe", ack, "activation-probe-answer", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := x.f.store.SettleTurnByID(ctx, turnID, domain.TurnStateCompleted, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		payload := fmt.Sprintf(`{"providerTurnId":%q,"providerConversationId":%q,"turnState":"completed"}`, "activation-provider-probe", op.ResumeEvidence.AfterNativeID)
		if _, err := x.f.store.ProjectProviderEvent(ctx, conv.ID, target.ID, target.Metadata.ControllerGeneration, "activation-main-proof", "turn.completed", payload, time.Now(), func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		turn, err := x.f.store.TurnByID(ctx, turnID)
		if err != nil {
			t.Fatal(err)
		}
		x.f.runtime.successorObservation.ControlTurn = &turn
		x.f.runtime.successorObservation.ControlAcknowledged = true
		return replacement.PreparationDelivery{TurnID: turnID, Owner: op.ResumeEvidence.AfterOwner, NativeID: op.ResumeEvidence.AfterNativeID}, nil
	}
	x.operation, err = x.service().Advance(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !settle {
		return x
	}
	x.operation, err = x.service().Advance(ctx, op.ID)
	if err != nil || x.operation.Phase != domain.ReplacementActivatingSharedWriter {
		t.Fatal("fixture did not verify actual probe", x.operation.Phase, x.operation.Error, err)
	}
	return x
}

type lostActivationResponse struct {
	*docs.Service
	calls int
}

func (d *lostActivationResponse) ActivateReplacementWriter(ctx context.Context, id string, revision int64) (domain.OrchestratorReplacement, error) {
	d.calls++
	op, err := d.Service.ActivateReplacementWriter(ctx, id, revision)
	if err != nil {
		return op, err
	}
	return domain.OrchestratorReplacement{}, errors.New("activation reply lost after commit")
}

func TestReplacementCoordinatorRecoversCommittedActivationResponse(t *testing.T) {
	x := activationCoordinatorFixture(t)
	ctx := context.Background()
	before, err := x.f.store.GetProjectDocumentState(ctx, x.operation.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	documents := &lostActivationResponse{Service: x.documents}
	service := replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: x.f.runtime, Documents: documents, NativeTranscripts: x.reader})
	op, err := service.Advance(ctx, x.operation.ID)
	if err != nil || op.Phase != domain.ReplacementNotifying || op.Error != nil || op.WriterActivation == nil || documents.calls != 1 {
		t.Fatal("lost committed activation discarded receipt", op.Phase, op.Error, err)
	}
	if _, err := x.documents.ActivateReplacementWriter(ctx, op.ID, op.Revision); err != nil {
		t.Fatal("idempotent replay failed", err)
	}
	again, err := service.Advance(ctx, op.ID)
	if err != nil || again.Phase != domain.ReplacementNotifying || again.Revision != op.Revision+2 || again.IntakeReleasedAt == nil || again.IntakeDrainRequestedAt == nil || documents.calls != 1 {
		t.Fatal("pending notification boundary repeated activation or became failure", again.Phase, err)
	}
	after, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
	if err != nil || after.Lease.Epoch != before.Lease.Epoch+1 {
		t.Fatal("lost response incremented lease twice", err)
	}
}

type activationRaceStore struct {
	*sqlite.Store
	before func()
}

func (s activationRaceStore) WithReplacementDocumentPublication(ctx context.Context, id string, revision int64, target domain.SessionRecord, documentRevision int64, publish func(context.Context) error) (bool, error) {
	s.before()
	return s.Store.WithReplacementDocumentPublication(ctx, id, revision, target, documentRevision, publish)
}

func TestReplacementActivationRejectsChangesBetweenHashAndReservation(t *testing.T) {
	for _, change := range []string{"generation", "capability", "probe reply", "probe child", "prefix preserve mtime"} {
		t.Run(change, func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			op := x.operation
			before, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			store := activationRaceStore{Store: x.f.store.Store, before: func() {
				rec, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
				if err != nil {
					t.Fatal(err)
				}
				switch change {
				case "generation":
					rec.Metadata.ControllerGeneration = "rotated-before-reservation"
					if err := x.f.store.UpdateSession(ctx, rec); err != nil {
						t.Fatal(err)
					}
				case "capability":
					if applied, err := x.f.store.UpdateBrowserCapabilityVerifier(ctx, rec.ID, rec.ControllerOwner(), "rotated-before-reservation"); err != nil || !applied {
						t.Fatal(applied, err)
					}
				case "probe reply":
					if err := x.f.store.SettleAssistantMessage(ctx, x.conversation.ID, "activation-probe-answer-item", "activation-provider-probe", "changed after preflight", "activation-probe-answer", time.Now()); err != nil {
						t.Fatal(err)
					}
				case "probe child":
					payload := `{"providerTurnId":"activation-provider-probe","providerConversationId":"child-native"}`
					if _, err := x.f.store.ProjectProviderEvent(ctx, x.conversation.ID, rec.ID, rec.Metadata.ControllerGeneration, "child-before-reservation", "turn.started", payload, time.Now(), func(context.Context) error { return nil }); err != nil {
						t.Fatal(err)
					}
				case "prefix preserve mtime":
					path := filepath.Join(op.Transcript.Receipt.Directory, "transcript.md")
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					file, err := os.OpenFile(path, os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := file.WriteAt([]byte("X"), 0)
					closeErr := file.Close()
					if writeErr != nil || closeErr != nil {
						t.Fatal(writeErr, closeErr)
					}
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
			}}
			service := docs.New(docs.Deps{Store: store, DataDir: x.documentDataDir, Capabilities: sourceCapability{}})
			if _, err := service.ActivateReplacementWriter(ctx, op.ID, op.Revision); err == nil {
				t.Fatal("changed evidence/owner activated writer", change)
			}
			after, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("failed final check moved lease", err)
			}
		})
	}
}

func TestReplacementActivationReceiptCannotBeForgedByGenericSave(t *testing.T) {
	x := activationCoordinatorFixture(t)
	op := x.operation
	state, err := x.f.store.GetProjectDocumentState(context.Background(), op.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementNotifying
	op.WriterActivation = &domain.ReplacementWriterActivation{BeforeEpoch: state.Lease.Epoch, AfterEpoch: state.Lease.Epoch + 1, Owner: op.ResumeEvidence.AfterOwner, CapabilityVerifier: op.ResumeEvidence.AfterCapabilityVerifier, ActivatedAt: time.Now().UTC()}
	if saved, err := x.f.store.SaveOrchestratorReplacement(context.Background(), op); saved || err == nil {
		t.Fatal("generic save forged an activation while source lease stayed frozen", saved, err)
	}
}

func TestReplacementActivationRejectsOversizedNonceReply(t *testing.T) {
	x := activationCoordinatorFixture(t)
	op := x.operation
	ack, ok := domain.ReplacementProbeAcknowledgment(op)
	if !ok {
		t.Fatal("missing nonce")
	}
	if err := x.f.store.SettleAssistantMessage(context.Background(), x.conversation.ID, "activation-probe-answer-item", "activation-provider-probe", ack+strings.Repeat("x", domain.MaxReplacementProbeReplyBytes), "activation-probe-answer", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := x.documents.ActivateReplacementWriter(context.Background(), op.ID, op.Revision); err == nil {
		t.Fatal("oversized control reply activated writer")
	}
}

type activationCapability struct{ target domain.SessionID }

func (c activationCapability) Valid(id domain.SessionID, token, verifier string) bool {
	return id == c.target && token == "successor-secret" && verifier == "after-verifier" || token == "author-secret" && verifier == "verifier"
}

func TestReplacementWriterActivationCommitsEpochAndBoundaryAtomically(t *testing.T) {
	for _, change := range []string{"activate", "handoff.md", "transcript.md", "manifest.json", "living changed", "owner rotated", "capability rotated", "probe reply changed", "probe failed", "child conflict", "lease ENOSPC", "operation ENOSPC", "new worker"} {
		t.Run(change, func(t *testing.T) {
			x := activationCoordinatorFixture(t)
			ctx := context.Background()
			op := x.operation
			before, err := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			target, _, err := x.f.store.GetSession(ctx, op.SuccessorSessionID)
			if err != nil {
				t.Fatal(err)
			}
			worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "handoff.md", "transcript.md", "manifest.json":
				if err := os.WriteFile(filepath.Join(op.Transcript.Receipt.Directory, change), []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "living changed":
				if err := os.WriteFile(filepath.Join(op.SharedDir, op.DocumentPath), []byte("unmanaged rewrite"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "owner rotated":
				target.Metadata.ControllerGeneration = "another-generation"
				if err := x.f.store.UpdateSession(ctx, target); err != nil {
					t.Fatal(err)
				}
			case "capability rotated":
				if applied, err := x.f.store.UpdateBrowserCapabilityVerifier(ctx, target.ID, target.ControllerOwner(), "another-verifier"); err != nil || !applied {
					t.Fatal(applied, err)
				}
			case "probe reply changed":
				if err := x.f.store.SettleAssistantMessage(ctx, x.conversation.ID, "activation-probe-answer-item", "activation-provider-probe", "no operation nonce", "activation-probe-answer", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "probe failed":
				if err := x.f.store.SettleTurnByID(ctx, op.ResumeEvidence.Probe.TurnID, domain.TurnStateFailed, "late failure", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "child conflict":
				payload := `{"providerTurnId":"activation-provider-probe","providerConversationId":"child-native"}`
				if _, err := x.f.store.ProjectProviderEvent(ctx, x.conversation.ID, target.ID, target.Metadata.ControllerGeneration, "activation-child-conflict", "turn.started", payload, time.Now(), func(context.Context) error { return nil }); err != nil {
					t.Fatal(err)
				}
			case "lease ENOSPC", "operation ENOSPC":
				raw, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				trigger := `CREATE TRIGGER reject_activation BEFORE UPDATE ON project_document_states WHEN json_extract(NEW.state_json,'$.lease.sessionId')='` + string(target.ID) + `' BEGIN SELECT RAISE(ABORT,'lease ENOSPC'); END`
				if change == "operation ENOSPC" {
					trigger = `CREATE TRIGGER reject_activation BEFORE UPDATE ON orchestrator_replacements WHEN NEW.phase='notifying' BEGIN SELECT RAISE(ABORT,'operation ENOSPC'); END`
				}
				if _, err := raw.ExecContext(ctx, trigger); err != nil {
					t.Fatal(err)
				}
			case "new worker":
				if _, err := x.f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := x.documents.ActivateReplacementWriter(ctx, op.ID, op.Revision)
			after, readErr := x.f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			stored, _, readErr := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if change == "activate" || change == "new worker" {
				if err != nil || result.Phase != domain.ReplacementNotifying || stored.WriterActivation == nil || after.Lease.SessionID != target.ID || after.Lease.Owner != op.ResumeEvidence.AfterOwner || after.Lease.Epoch != before.Lease.Epoch+1 || after.Lease.Frozen || after.Lease.ClaimVerifier != "after-verifier" || after.Lease.TokenHash != "" || stored.IntakeReleasedAt != nil {
					t.Fatal("activation not atomic/pending claim", result.Phase, err)
				}
				want := 1
				if change == "new worker" {
					want = 2
				}
				if len(stored.Notifications) != want {
					t.Fatal("worker notification boundary lost", len(stored.Notifications), want)
				}
				for _, notice := range stored.Notifications {
					if notice.State != "pending" || notice.ContactID == "" {
						t.Fatal("notification acceptance fabricated", notice)
					}
				}
				for _, mutation := range []string{"activation time", "epoch", "drop notifications", "complete pending"} {
					candidate, _, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
					if err != nil {
						t.Fatal(err)
					}
					switch mutation {
					case "activation time":
						candidate.WriterActivation.ActivatedAt = candidate.WriterActivation.ActivatedAt.Add(time.Second)
					case "epoch":
						candidate.WriterActivation.AfterEpoch++
					case "drop notifications":
						candidate.Notifications = nil
					case "complete pending":
						now := time.Now().UTC()
						candidate.IntakeReleasedAt = &now
						candidate.Phase = domain.ReplacementCompleted
					}
					if saved, err := x.f.store.SaveOrchestratorReplacement(ctx, candidate); saved || err == nil {
						t.Fatal("activation/notification facts rewritten", mutation, saved, err)
					}
				}
				service := docs.New(docs.Deps{Store: x.f.store.Store, DataDir: x.documentDataDir, Capabilities: activationCapability{target: target.ID}})
				actor := docs.Actor{SessionID: target.ID, Capability: "successor-secret"}
				grant, claimErr := service.Acquire(ctx, op.ProjectID, actor)
				if claimErr != nil || grant.Epoch != after.Lease.Epoch {
					t.Fatal("successor could not claim its epoch", claimErr)
				}
				if _, writeErr := service.Write(ctx, op.ProjectID, docs.WriteInput{Actor: actor, Lease: grant, Path: op.DocumentPath, ExpectedHash: op.Preparation.SubmittedHash, Content: "new current orchestrator"}); writeErr != nil {
					t.Fatal(writeErr)
				}
				if _, oldErr := service.Acquire(ctx, op.ProjectID, docs.Actor{SessionID: op.SourceSessionID, Capability: "author-secret"}); oldErr == nil {
					t.Fatal("retired source reclaimed writer")
				}
			} else if err == nil || !reflect.DeepEqual(before, after) || stored.Phase != domain.ReplacementActivatingSharedWriter || stored.WriterActivation != nil {
				t.Fatal("failed activation changed lease/operation", change, err, stored.Phase)
			}
			currentWorker, _, readErr := x.f.store.GetSession(ctx, worker.ID)
			if readErr != nil || !reflect.DeepEqual(worker, currentWorker) {
				t.Fatal("activation touched worker", readErr)
			}
		})
	}
}
