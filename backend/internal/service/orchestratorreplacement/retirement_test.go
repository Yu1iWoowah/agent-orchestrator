package orchestratorreplacement_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReplacementRetirementDurableIntentAndCommitSurviveRestart(t *testing.T) {
	for _, loss := range []string{"none", "lost result", "lost coordinator"} {
		t.Run(loss, func(t *testing.T) {
			x := preparedExportFixture(t)
			ctx := context.Background()
			x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
			receipt := *x.operation.Transcript.Receipt
			x.f.runtime.retire = func(ctx context.Context, op domain.OrchestratorReplacement, proof ports.ReplacementRetirementProof) (domain.OrchestratorReplacement, error) {
				persisted, found, err := x.f.store.GetOrchestratorReplacement(ctx, op.ID)
				if err != nil || !found || persisted.Phase != domain.ReplacementRetiring || persisted.Revision != op.Revision || proof.OperationID() != op.ID || proof.OperationRevision() != op.Revision {
					t.Fatal("retirement preceded durable intent", err)
				}
				result, applied, err := x.f.store.CommitReplacementRetirement(ctx, op.ID, op.Revision, proof.Source(), proof.DocumentRevision(), proof.Check)
				if err != nil || !applied {
					return result, err
				}
				if loss == "lost coordinator" {
					panic("lost coordinator after atomic retirement")
				}
				if loss == "lost result" {
					return domain.OrchestratorReplacement{}, errors.New("lost retirement response")
				}
				return result, nil
			}
			intent, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil || intent.Phase != domain.ReplacementRetiring || intent.SourceRetiredAt != nil || x.f.runtime.retirements != 0 {
				t.Fatalf("no separate retirement intent: phase=%s retirements=%d err=%v", intent.Phase, x.f.runtime.retirements, err)
			}
			if _, err := x.f.store.AppendUserMessage(ctx, x.conversation.ID, x.source.ID, x.source.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "late-during-retirement", ClientMessageID: "late-retirement-client", Text: "preserve queued image", Origin: domain.MessageOriginHuman, DeliveryContentJSON: `[{"type":"image","data":"aW1hZ2U=","mimeType":"image/png"}]`}, "late-retirement-turn", time.Now()); err != nil {
				t.Fatal(err)
			}
			queued, found, err := x.f.store.ConversationMessageByClientID(ctx, x.conversation.ID, "late-retirement-client")
			if err != nil || !found {
				t.Fatal("late message was not durably accepted", err)
			}
			if loss == "lost coordinator" {
				var crashed any
				func() { defer func() { crashed = recover() }(); _, _ = x.service().Advance(ctx, intent.ID) }()
				if crashed != "lost coordinator after atomic retirement" {
					t.Fatalf("wrong crash boundary: %v", crashed)
				}
			} else {
				result, err := x.service().Advance(ctx, intent.ID)
				if err != nil || result.Phase != domain.ReplacementStarting || result.SourceRetiredAt == nil {
					t.Fatalf("retirement result not reconciled: phase=%s err=%v", result.Phase, err)
				}
			}
			result, err := x.service().Get(ctx, intent.ID)
			if err != nil || result.Phase != domain.ReplacementStarting || result.SourceRetiredAt == nil || result.Transcript.Receipt == nil || *result.Transcript.Receipt != receipt {
				t.Fatal("restart lost retirement/evidence facts", err)
			}
			source, found, err := x.f.store.GetSession(ctx, x.source.ID)
			if err != nil || !found || !source.IsTerminated || source.Metadata.ProviderConversationID != x.source.Metadata.ProviderConversationID {
				t.Fatal("atomic retirement did not preserve native identity", err)
			}
			worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil || !reflect.DeepEqual(worker, x.worker) || x.f.runtime.quiescences != 1 || *x.actualStops != 1 || x.f.runtime.retirements != 1 {
				t.Fatal("retirement touched worker or repeated source stop", err)
			}
			after, found, err := x.f.store.ConversationMessageByClientID(ctx, x.conversation.ID, "late-retirement-client")
			if err != nil || !found || !reflect.DeepEqual(after, queued) {
				t.Fatal("retirement/restart changed queued message or attachments", err)
			}
		})
	}
}

func TestReplacementRetirementRejectsChangedEvidenceAndUnconfirmedRuntime(t *testing.T) {
	for _, change := range []string{"native prefix", "manifest", "living handoff", "native append after proof", "owner changes at runtime", "stop uncertain", "missing durable commit", "target unavailable", "target account changed"} {
		t.Run(change, func(t *testing.T) {
			x := preparedExportFixture(t)
			ctx := context.Background()
			x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
			intent, err := x.service().Advance(ctx, x.operation.ID)
			if err != nil || intent.Phase != domain.ReplacementRetiring {
				t.Fatal("missing retirement intent", err)
			}
			path := ""
			switch change {
			case "native prefix":
				data, err := os.ReadFile(x.nativePath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(x.nativePath, []byte(strings.Replace(string(data), "original user request", "modified user request", 1)), 0o600); err != nil {
					t.Fatal(err)
				}
			case "manifest":
				path = filepath.Join(intent.Transcript.Receipt.Directory, "manifest.json")
			case "living handoff":
				path = filepath.Join(intent.SharedDir, intent.DocumentPath)
			case "target unavailable":
				x.f.runtime.err = errors.New("target no longer authenticated")
			case "target account changed":
				x.f.runtime.target = &domain.ReplacementTarget{AccountID: "different-account", AgentConfig: intent.TargetAgentConfig}
			}
			if path != "" {
				if err := os.WriteFile(path, []byte("changed evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			x.f.runtime.retire = func(ctx context.Context, op domain.OrchestratorReplacement, proof ports.ReplacementRetirementProof) (domain.OrchestratorReplacement, error) {
				if change == "owner changes at runtime" {
					source, _, err := x.f.store.GetSession(ctx, op.SourceSessionID)
					if err != nil {
						t.Fatal(err)
					}
					source.Metadata.ControllerGeneration = "rotated-during-retirement"
					if err := x.f.store.UpdateSession(ctx, source); err != nil {
						t.Fatal(err)
					}
					return domain.OrchestratorReplacement{}, domain.ErrReplacementSourceChanged
				}
				if change == "stop uncertain" {
					return domain.OrchestratorReplacement{}, ports.ErrChatRecoveryInconclusive
				}
				if change == "missing durable commit" {
					return op, nil
				}
				if change != "native append after proof" {
					t.Fatal("invalid preflight reached runtime retirement")
				}
				file, err := os.OpenFile(x.nativePath, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteString(`{"type":"system","subtype":"stop_hook_summary","preventedContinuation":true}` + "\n")
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal(writeErr, closeErr)
				}
				result, _, err := x.f.store.CommitReplacementRetirement(ctx, op.ID, op.Revision, proof.Source(), proof.DocumentRevision(), proof.Check)
				return result, err
			}
			result, err := x.service().Advance(ctx, intent.ID)
			wantPhase := domain.ReplacementRecoveryRequired
			if err != nil || result.Phase != wantPhase || result.ResumePhase != domain.ReplacementRetiring || result.Error == nil || result.SourceRetiredAt != nil {
				t.Fatalf("%s not visibly stopped: phase=%s error=%+v err=%v", change, result.Phase, result.Error, err)
			}
			if change == "stop uncertain" && result.Error.Code != "ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN" {
				t.Fatalf("stop uncertainty lost its named error: %s", result.Error.Code)
			}
			if result.TargetAccountID != intent.TargetAccountID || result.Transcript.Receipt == nil || *result.Transcript.Receipt != *intent.Transcript.Receipt {
				t.Fatal("failure rewrote pinned target/evidence")
			}
			source, found, err := x.f.store.GetSession(ctx, x.source.ID)
			if err != nil || !found || source.IsTerminated {
				t.Fatal("unverified retirement terminated source", err)
			}
			wantCalls := 0
			if change == "native append after proof" || change == "stop uncertain" || change == "missing durable commit" || change == "owner changes at runtime" {
				wantCalls = 1
			}
			if x.f.runtime.retirements != wantCalls || *x.actualStops != 1 {
				t.Fatal("invalid preflight reached retirement or repeated source stop")
			}
			worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil || !reflect.DeepEqual(worker, x.worker) {
				t.Fatal("failure changed worker", err)
			}
		})
	}
}
