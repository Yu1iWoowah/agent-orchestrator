package orchestratorreplacement_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type exportReader struct {
	ports.NativeTranscriptReader
	capturer ports.NativeTranscriptCapturer
	captures int
}

func (r *exportReader) CaptureNativeTranscript(ctx context.Context, request ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	r.captures++
	return r.capturer.CaptureNativeTranscript(ctx, request)
}

func nativePreparationFile(t *testing.T, path, native, prompt string) {
	t.Helper()
	rows := []map[string]any{
		{"uuid": "old-u", "parentUuid": nil, "type": "user", "promptId": "old-prompt", "message": map[string]any{"content": "original user request"}},
		{"uuid": "old-a", "parentUuid": "old-u", "type": "assistant", "promptId": "old-prompt", "message": map[string]any{"content": "original answer", "stop_reason": "end_turn"}},
		{"uuid": "prepare-u", "parentUuid": "old-a", "type": "user", "promptId": "preparation-provider-turn", "message": map[string]any{"content": prompt}},
		{"uuid": "prepare-a", "parentUuid": "prepare-u", "type": "assistant", "promptId": "preparation-provider-turn", "message": map[string]any{"content": "operation handoff prepared", "stop_reason": "end_turn"}},
		{"uuid": "child-a", "parentUuid": "prepare-a", "type": "assistant", "isSidechain": true, "agentId": "child-agent", "message": map[string]any{"content": "child-private-text", "stop_reason": "end_turn"}},
	}
	var body strings.Builder
	for _, row := range rows {
		row["sessionId"], row["version"] = native, "2.1.0"
		if row["isSidechain"] == nil {
			row["isSidechain"] = false
		}
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(data)
		body.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

type preparedExport struct {
	f              fixture
	source, worker domain.SessionRecord
	conversation   domain.ConversationRecord
	nativePath     string
	documents      *projectdocuments.Service
	reader         *exportReader
	operation      domain.OrchestratorReplacement
	actualStops    *int

	documentDataDir string
}

func (x *preparedExport) service() *replacement.Service {
	return replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader})
}

func (x *preparedExport) advanceUntil(t *testing.T, done func(domain.OrchestratorReplacement) bool) {
	t.Helper()
	for range 16 {
		if done(x.operation) {
			return
		}
		next, err := x.service().Advance(context.Background(), x.operation.ID)
		x.operation = next
		if err != nil || next.Error != nil {
			t.Fatalf("advance phase=%s err=%v failure=%+v", next.Phase, err, next.Error)
		}
	}
	t.Fatal("export boundary did not converge", x.operation.Phase)
}

func preparedExportFixture(t *testing.T) *preparedExport {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	native := "11111111-1111-4111-8111-111111111111"
	source := f.source
	source.Metadata.ProviderConversationID = native
	if err := f.store.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	f.runtime.observation.Owner, f.runtime.observation.NativeID = source.ControllerOwner(), native
	worker, err := f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: source.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: time.Now()}, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{ControllerGeneration: "worker-generation", ProviderConversationID: "worker-native"}})
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err = f.store.GetSession(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := f.store.CreateConversation(ctx, "project-conversation", domain.ConversationScopeProject, source.ProjectID, source.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	nativePath := filepath.Join(root, "projects", "workspace", native+".jsonl")
	binding, err := f.store.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: source.ID, Harness: domain.HarnessClaudeCode, NativeRootID: native})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.InsertUsageSource(ctx, domain.UsageSourceRecord{BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain, NativeSessionID: native, ArtifactPath: nativePath, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	documentDataDir := t.TempDir()
	documents := projectdocuments.New(projectdocuments.Deps{Store: f.store.Store, DataDir: documentDataDir, Capabilities: sourceCapability{}})
	agent := claudecode.New()
	reader := &exportReader{NativeTranscriptReader: agent, capturer: agent}
	makeService := func() *replacement.Service {
		return replacement.New(replacement.Deps{Store: f.store.Store, Runtime: f.runtime, Documents: documents, NativeTranscripts: reader})
	}
	f.runtime.prepare = func(ctx context.Context, op domain.OrchestratorReplacement, prompt string, _ bool) (replacement.PreparationDelivery, error) {
		now := time.Now().UTC()
		if _, err := f.store.AppendUserMessage(ctx, conv.ID, source.ID, source.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "preparation-message", ClientMessageID: op.Preparation.ControlClientID, Text: prompt, Origin: domain.MessageOriginAutomation}, "preparation-turn", now); err != nil {
			t.Fatal(err)
		}
		if err := f.store.RecordProviderInput(ctx, "preparation-turn", prompt); err != nil {
			t.Fatal(err)
		}
		if err := f.store.BindTurnToProvider(ctx, "preparation-turn", "preparation-provider-turn", now); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(map[string]string{"providerTurnId": "preparation-provider-turn", "providerConversationId": native})
		if _, err := f.store.ProjectProviderEvent(ctx, conv.ID, source.ID, source.Metadata.ControllerGeneration, "preparation-native-evidence", "turn.completed", string(payload), now, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := f.store.SettleTurnByID(ctx, "preparation-turn", domain.TurnStateCompleted, "", now); err != nil {
			t.Fatal(err)
		}
		f.runtime.observation.ControlTurn = &domain.ConversationTurn{ID: "preparation-turn", HandledBySessionID: source.ID, State: domain.TurnStateCompleted}
		nativePreparationFile(t, nativePath, native, prompt)
		return replacement.PreparationDelivery{TurnID: "preparation-turn", Owner: op.SourceOwner, NativeID: native}, nil
	}
	actualStops := 0
	var stopped *domain.ReplacementControllerStop
	f.runtime.quiesce = func(ctx context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
		if op.SourceStopIntentAt == nil || op.FrozenAt == nil || op.Preparation.VerifiedAt == nil {
			t.Fatal("provider action preceded durable preparation/stop intent")
		}
		rec, _, err := f.store.GetSession(ctx, source.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rec.HibernatedAt != nil {
			if stopped == nil {
				return domain.ReplacementControllerStop{}, errors.New("marker alone does not prove shutdown")
			}
			return *stopped, nil
		}
		actualStops++
		now := time.Now().UTC()
		if ok, err := f.store.SetSessionHibernated(ctx, source.ID, rec.Revision, &now); err != nil || !ok {
			t.Fatal(ok, err)
		}
		stopped = &domain.ReplacementControllerStop{Owner: op.SourceOwner, NativeID: native, CapabilityVerifier: op.SourceCapabilityVerifier, Method: "chat-hibernate", ConfirmedAt: now}
		return *stopped, nil
	}
	op, err := makeService().Accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if op.Preparation != nil && op.Preparation.ControlTurnID != "" {
			break
		}
		op, err = makeService().Advance(ctx, op.ID)
		if err != nil || op.Error != nil {
			t.Fatal(err, op.Error)
		}
	}
	actor := projectdocuments.Actor{SessionID: source.ID, Capability: "author-secret"}
	grant, err := documents.Acquire(ctx, source.ProjectID, actor)
	if err != nil {
		t.Fatal(err)
	}
	file, err := documents.Write(ctx, source.ProjectID, projectdocuments.WriteInput{Actor: actor, Lease: grant, Path: op.DocumentPath, Content: operationHandoff(t, op), ExpectedHash: projectdocuments.MissingHash})
	if err != nil {
		t.Fatal(err)
	}
	op, err = makeService().Prepared(ctx, op.ID, projectdocuments.ReplacementPreparationInput{Actor: actor, AttemptID: op.Preparation.AttemptID, Nonce: op.Preparation.Nonce, Path: op.DocumentPath, Hash: file.Hash, WriteReceiptID: file.WriteReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	return &preparedExport{f: f, source: source, worker: worker, conversation: conv, nativePath: nativePath, documents: documents, documentDataDir: documentDataDir, reader: reader, operation: op, actualStops: &actualStops}
}

func TestReplacementExportPersistsBoundariesAcrossCoordinatorRestartWithoutRetiring(t *testing.T) {
	x := preparedExportFixture(t)
	f, ctx, source, worker, conv, documents, reader, op := x.f, context.Background(), x.source, x.worker, x.conversation, x.documents, x.reader, x.operation
	makeService := x.service
	var err error
	lateQueued := false
	for range 14 {
		if op.Phase == domain.ReplacementTranscriptVerified {
			break
		}
		if op.FrozenAt != nil && !lateQueued {
			lateQueued = true
			if _, err := f.store.AppendUserMessage(ctx, conv.ID, source.ID, source.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "late-input", ClientMessageID: "late-client", Text: "successor-only queued input", Origin: domain.MessageOriginHuman, DeliveryContentJSON: `[{"type":"image","data":"aW1hZ2U=","mimeType":"image/png"}]`}, "late-turn", time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		op, err = makeService().Advance(ctx, op.ID)
		if err != nil || op.Error != nil {
			t.Fatalf("export boundary failed: phase=%s error=%v failure=%+v", op.Phase, err, op.Error)
		}
	}
	if op.Phase != domain.ReplacementTranscriptVerified || op.Transcript == nil || op.Transcript.Receipt == nil || op.SourceStop == nil || op.SourceRetiredAt != nil || f.runtime.quiescences != 1 || reader.captures != 1 {
		t.Fatalf("export facts missing/repeated: phase=%s stops=%d captures=%d", op.Phase, f.runtime.quiescences, reader.captures)
	}
	exported, err := documents.ReadEvidence(ctx, source.ProjectID, op.ID, op.Transcript.AttemptID, "transcript.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exported.Content, "preparing_handoff control turn") || !strings.Contains(exported.Content, "original user request") || strings.Contains(exported.Content, "child-private-text") || strings.Contains(exported.Content, "successor-only queued input") {
		t.Fatal("preparation/main/child/cutoff scope mixed")
	}
	stored, found, err := f.store.ConversationMessageByClientID(ctx, conv.ID, "late-client")
	if err != nil || !found || stored.ID != "late-input" || stored.DeliveryContentJSON == "" {
		t.Fatal("post-cutoff receipt/attachment lost", err)
	}
	after, _, err := f.store.GetSession(ctx, worker.ID)
	if err != nil || !reflect.DeepEqual(after, worker) {
		t.Fatal("export touched active worker", err)
	}
	current, _, err := f.store.GetSession(ctx, source.ID)
	if err != nil || current.IsTerminated {
		t.Fatal("export retired source", err)
	}
}

type readOnlyExportReader struct{ ports.NativeTranscriptReader }

func TestReplacementExportFailsClosedWithoutRetirement(t *testing.T) {
	for _, which := range []string{"missing capture", "busy", "uncertain stop", "wrong native stop", "changed native prefix", "changed AO outcome", "deadline"} {
		t.Run(which, func(t *testing.T) {
			x := preparedExportFixture(t)
			ctx := context.Background()
			x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementExportingTranscript })
			if _, err := x.f.store.AppendUserMessage(ctx, x.conversation.ID, x.source.ID, x.source.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "held-after-cutoff", ClientMessageID: "held-after-cutoff", Text: "preserve next instruction", Origin: domain.MessageOriginHuman}, "held-after-cutoff-turn", time.Now()); err != nil {
				t.Fatal(err)
			}
			svc := x.service()
			wantPhase := domain.ReplacementAwaitingTranscript
			wantCode := ""
			switch which {
			case "missing capture":
				svc = replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: readOnlyExportReader{x.reader}})
				wantCode = "TRANSCRIPT_CAPTURE_UNAVAILABLE"
			case "deadline":
				expired := x.operation.DeadlineAt.Add(time.Second)
				svc = replacement.New(replacement.Deps{Store: x.f.store.Store, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader, Now: func() time.Time { return expired }})
				wantPhase, wantCode = domain.ReplacementAwaitingSource, "ORCHESTRATOR_HANDOFF_SOURCE_IDLE_TIMEOUT"
			case "changed native prefix", "changed AO outcome":
				x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Transcript.CapturedAt != nil })
				if which == "changed native prefix" {
					data, err := os.ReadFile(x.nativePath)
					if err != nil {
						t.Fatal(err)
					}
					changed := strings.Replace(string(data), "original user request", "modified user request", 1)
					if len(changed) != len(data) {
						t.Fatal("mutation changed size")
					}
					if err := os.WriteFile(x.nativePath, []byte(changed), 0o600); err != nil {
						t.Fatal(err)
					}
					wantCode = "TRANSCRIPT_SOURCE_CHANGED"
				} else {
					if err := x.f.store.SettleTurnByID(ctx, "preparation-turn", domain.TurnStateCompleted, "changed included outcome", time.Now()); err != nil {
						t.Fatal(err)
					}
					wantCode = "HANDOFF_EVIDENCE_SOURCE_CHANGED"
				}
			default:
				x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
				original := x.f.runtime.quiesce
				switch which {
				case "busy":
					x.f.runtime.quiesce = func(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
						return domain.ReplacementControllerStop{}, domain.ErrReplacementSourceBusy
					}
					wantPhase, wantCode = domain.ReplacementExportingTranscript, "ORCHESTRATOR_HANDOFF_SOURCE_BUSY"
				case "uncertain stop":
					x.f.runtime.quiesce = func(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
						return domain.ReplacementControllerStop{}, errors.New("unconfirmed transport: fixture-secret")
					}
					wantPhase, wantCode = domain.ReplacementRecoveryRequired, "ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN"
				case "wrong native stop":
					x.f.runtime.quiesce = func(ctx context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
						stop, err := original(ctx, op)
						stop.NativeID = "another-native"
						return stop, err
					}
					wantPhase, wantCode = domain.ReplacementRecoveryRequired, "ORCHESTRATOR_HANDOFF_SOURCE_STOP_UNCERTAIN"
				}
			}
			result, err := svc.Advance(ctx, x.operation.ID)
			if err != nil || result.Phase != wantPhase || result.Error == nil || result.Error.Code != wantCode || result.SourceRetiredAt != nil || result.Transcript.Receipt != nil {
				t.Fatalf("%s: phase=%s error=%v failure=%+v", which, result.Phase, err, result.Error)
			}
			if strings.Contains(result.Error.Message, "fixture-secret") {
				t.Fatal("provider error leaked into operation")
			}
			if which == "busy" && (*x.actualStops != 0 || x.reader.captures != 0) {
				t.Fatal("busy source was stopped or captured")
			}
			if which == "missing capture" || which == "deadline" {
				if x.f.runtime.quiescences != 0 || result.SourceStopIntentAt != nil {
					t.Fatal("unavailable/expired source reached stop")
				}
			}
			if which == "uncertain stop" || which == "wrong native stop" {
				calls := x.f.runtime.quiescences
				if _, err := x.service().Advance(ctx, result.ID); err != nil || x.f.runtime.quiescences != calls {
					t.Fatal("recovery blindly retried stop", err)
				}
			}
			held, found, err := x.f.store.ConversationMessageByClientID(ctx, x.conversation.ID, "held-after-cutoff")
			if err != nil || !found || held.Text != "preserve next instruction" {
				t.Fatal("failed export lost queued intake", err)
			}
			source, _, err := x.f.store.GetSession(ctx, x.source.ID)
			if err != nil || source.IsTerminated {
				t.Fatal("failed export retired source", err)
			}
			worker, _, err := x.f.store.GetSession(ctx, x.worker.ID)
			if err != nil || !reflect.DeepEqual(worker, x.worker) {
				t.Fatal("failed export touched worker", err)
			}
		})
	}
}

type crashAfterSourceStopStore struct {
	*sqlite.Store
	crash bool
}

func (s *crashAfterSourceStopStore) SaveOrchestratorReplacement(ctx context.Context, op domain.OrchestratorReplacement) (bool, error) {
	if s.crash && op.SourceStop != nil {
		s.crash = false
		panic("lost coordinator after provider stop")
	}
	return s.Store.SaveOrchestratorReplacement(ctx, op)
}

func TestReplacementExportRecoversSourceStopBeforeReceiptSave(t *testing.T) {
	x := preparedExportFixture(t)
	ctx := context.Background()
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.SourceStopIntentAt != nil })
	store := &crashAfterSourceStopStore{Store: x.f.store.Store, crash: true}
	svc := replacement.New(replacement.Deps{Store: store, Runtime: x.f.runtime, Documents: x.documents, NativeTranscripts: x.reader})
	var lost any
	func() { defer func() { lost = recover() }(); _, _ = svc.Advance(ctx, x.operation.ID) }()
	if lost != "lost coordinator after provider stop" {
		t.Fatalf("wrong crash boundary: %v", lost)
	}
	stored, found, err := x.f.store.GetOrchestratorReplacement(ctx, x.operation.ID)
	if err != nil || !found || stored.SourceStop != nil || stored.SourceStopIntentAt == nil || *x.actualStops != 1 {
		t.Fatal("stop/save gap not reproduced", err)
	}
	x.operation = stored
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.Phase == domain.ReplacementTranscriptVerified })
	if *x.actualStops != 1 || x.f.runtime.quiescences != 2 || x.reader.captures != 1 || x.operation.SourceRetiredAt != nil {
		t.Fatalf("physical stop repeated or export evidence lost: stops=%d checks=%d captures=%d", *x.actualStops, x.f.runtime.quiescences, x.reader.captures)
	}
}
