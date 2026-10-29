package orchestratorreplacement_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

func TestReplacementWaitsForPositiveIdleWithoutSendingOrStopping(t *testing.T) {
	for _, which := range []string{"busy", "approval", "unknown", "child", "active turn"} {
		t.Run(which, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			switch which {
			case "busy":
				f.runtime.observation.State = ports.ChatControllerBusy
			case "approval":
				f.runtime.observation.PendingApproval = true
			case "unknown":
				f.runtime.observation.State = ""
			case "child":
				f.runtime.observation.Provenance = domain.NativeThreadChild
			case "active turn":
				f.runtime.observation.ActiveTurn = &domain.ConversationTurn{ID: "current", HandledBySessionID: f.source.ID, State: domain.TurnStateRunning}
			}
			op, err := f.svc.Accept(ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			for range 4 {
				op, err = f.svc.Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if op.Phase != domain.ReplacementWaitingSourceIdle || op.Error == nil || f.runtime.preparations != 0 {
				t.Fatalf("unverified idle advanced: %+v sends=%d", op, f.runtime.preparations)
			}
			source, _, err := f.store.GetSession(ctx, f.source.ID)
			if err != nil || source.IsTerminated || source.ControllerOwner() != f.source.ControllerOwner() {
				t.Fatal("waiting changed source owner", err)
			}
		})
	}
}

func TestReplacementFreezeRequiresSuccessfulControlTurnAndReceipt(t *testing.T) {
	for _, which := range []string{"completed", "failed", "interrupted", "cancelled", "recovered", "approval", "missing receipt", "new worker", "new input", "new report"} {
		t.Run(which, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			var reportWorker domain.SessionRecord
			if which == "new report" {
				var err error
				reportWorker, err = f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: f.request.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: time.Now(), UpdatedAt: time.Now()})
				if err != nil {
					t.Fatal(err)
				}
			}
			documents := projectdocuments.New(projectdocuments.Deps{Store: f.store, DataDir: t.TempDir(), Capabilities: sourceCapability{}})
			svc := replacement.New(replacement.Deps{Store: f.store, Runtime: f.runtime, Documents: documents})
			op, err := svc.Accept(ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			for range 10 {
				if op.Preparation != nil && op.Preparation.ControlTurnID != "" {
					break
				}
				op, err = svc.Advance(ctx, op.ID)
				if err != nil || op.Error != nil {
					t.Fatal("preparation did not accept control turn", err, op.Error)
				}
			}
			if op.Preparation == nil || op.Preparation.ControlTurnID == "" {
				t.Fatal("control turn missing")
			}
			if which != "missing receipt" {
				actor := projectdocuments.Actor{SessionID: op.SourceSessionID, Capability: "author-secret"}
				grant, err := documents.Acquire(ctx, op.ProjectID, actor)
				if err != nil {
					t.Fatal(err)
				}
				file, err := documents.Write(ctx, op.ProjectID, projectdocuments.WriteInput{Actor: actor, Lease: grant, Path: op.DocumentPath, Content: operationHandoff(t, op), ExpectedHash: projectdocuments.MissingHash})
				if err != nil {
					t.Fatal(err)
				}
				op, err = svc.Prepared(ctx, op.ID, projectdocuments.ReplacementPreparationInput{Actor: actor, AttemptID: op.Preparation.AttemptID, Nonce: op.Preparation.Nonce, Path: op.DocumentPath, Hash: file.Hash, WriteReceiptID: file.WriteReceiptID})
				if err != nil {
					t.Fatal(err)
				}
			}
			state := domain.TurnStateCompleted
			if which == "failed" || which == "interrupted" || which == "cancelled" || which == "recovered" {
				state = domain.TurnState(which)
			}
			f.runtime.observation.ControlTurn = &domain.ConversationTurn{ID: op.Preparation.ControlTurnID, HandledBySessionID: op.SourceSessionID, State: state}
			f.runtime.observation.PendingApproval = which == "approval"
			var worker domain.SessionRecord
			if which == "new worker" {
				worker, err = f.store.CreateSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Metadata: domain.SessionMetadata{ControllerGeneration: "working-generation", ProviderConversationID: "working-native"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if which == "new input" {
				now := time.Now().UTC()
				conv, err := f.store.CreateConversation(ctx, "late-intake", domain.ConversationScopeProject, op.ProjectID, op.SourceSessionID, now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.AppendUserMessage(ctx, conv.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "new-instruction", ClientMessageID: "new-instruction", Text: "additional instruction", Origin: domain.MessageOriginHuman}, "late-turn", now); err != nil {
					t.Fatal(err)
				}
			}
			if which == "new report" {
				now := time.Now().UTC()
				if _, err := f.store.CreateReport(ctx, domain.ReportRecord{ID: "new-worker-question", SessionID: reportWorker.ID, ProjectID: op.ProjectID, State: domain.ReportNeedsInput, Note: "Q12: which option?", CreatedAt: now, AvailableAt: now, DeliveryState: domain.ReportPending, RepeatCount: 1}); err != nil {
					t.Fatal(err)
				}
			}
			op, err = svc.Advance(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if which == "new report" && (op.Error == nil || op.Error.Code != "ORCHESTRATOR_HANDOFF_INTAKE_CHANGED") {
				t.Fatalf("new worker question did not invalidate preparation: phase=%s failure=%+v", op.Phase, op.Error)
			}
			if which == "completed" {
				if op.Phase != domain.ReplacementHandoffVerified || op.Preparation.VerifiedAt == nil {
					t.Fatal("successful receipt/turn not verified", op.Phase)
				}
				for range 2 {
					op, err = svc.Advance(ctx, op.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				if op.Phase != domain.ReplacementExportingTranscript {
					t.Fatal("verified document not frozen for export", op.Phase, op.Error)
				}
			} else if which == "approval" {
				if op.Phase != domain.ReplacementPreparingHandoff || op.Preparation.VerifiedAt != nil {
					t.Fatal("approval skipped settlement barrier")
				}
			} else if op.Phase != domain.ReplacementAwaitingHandoff || op.Preparation.VerifiedAt != nil {
				t.Fatal("failed/incomplete turn advanced", op.Phase)
			}
			docState, err := f.store.GetProjectDocumentState(ctx, op.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			if docState.Lease.Frozen != (which == "completed") {
				t.Fatal("unverified control turn froze writer")
			}
			source, _, err := f.store.GetSession(ctx, op.SourceSessionID)
			if err != nil || source.IsTerminated || source.ControllerOwner() != f.source.ControllerOwner() {
				t.Fatal("source retired before transcript evidence", err)
			}
			if worker.ID != "" {
				after, _, err := f.store.GetSession(ctx, worker.ID)
				if err != nil || !reflect.DeepEqual(worker.ControllerOwner(), after.ControllerOwner()) || after.IsTerminated {
					t.Fatal("replacement interrupted active Codex worker", err)
				}
			}
		})
	}
}

func TestReplacementPreparationPromptIntentSurvivesRestartWithoutDuplicate(t *testing.T) {
	for _, crashed := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "after delivery before receipt"}[crashed], func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			documents := projectdocuments.New(projectdocuments.Deps{Store: f.store, DataDir: t.TempDir(), Capabilities: sourceCapability{}})
			svc := replacement.New(replacement.Deps{Store: f.store, Runtime: f.runtime, Documents: documents})
			op, err := svc.Accept(ctx, f.request)
			if err != nil {
				t.Fatal(err)
			}
			for op.Phase != domain.ReplacementPreparingHandoff {
				op, err = svc.Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if op.Error != nil {
					t.Fatal("unexpected wait before preparation", op.Error)
				}
			}
			if id, ok := domain.CoordinationDeliveryID(domain.WrapReportDelivery(op.Preparation.ControlClientID, "preparation")); !ok || id != op.Preparation.ControlClientID {
				t.Fatal("preparation control ID cannot use native semantic acceptance")
			}
			if crashed {
				now := time.Now().UTC()
				op.Preparation.PromptIntentAt = &now
				if ok, err := f.store.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
					t.Fatal(ok, err)
				}
				f.runtime.observation.ControlTurn = &domain.ConversationTurn{ID: "owned-control-turn", HandledBySessionID: op.SourceSessionID, State: domain.TurnStateRunning}
				f.runtime.observation.ActiveTurn = f.runtime.observation.ControlTurn
				f.runtime.observation.State = ports.ChatControllerBusy
			}
			// A new coordinator must recover the durable prompt key, not allocate a
			// fresh attempt merely because its caller/process disappeared.
			restarted := replacement.New(replacement.Deps{Store: f.store, Runtime: f.runtime, Documents: documents})
			for range 3 {
				op, err = restarted.Advance(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			wantSends := 1
			if crashed {
				wantSends = 0
			}
			if op.Phase != domain.ReplacementPreparingHandoff || op.Preparation.ControlTurnID != "owned-control-turn" || op.Preparation.PromptIntentAt == nil || f.runtime.preparations != wantSends {
				t.Fatalf("preparation replay lost intent/created another turn: %+v sends=%d", op, f.runtime.preparations)
			}
			if !crashed {
				hash := sha256.Sum256([]byte(f.runtime.prompts[0]))
				if op.Preparation.PromptHash != hex.EncodeToString(hash[:]) {
					t.Fatal("prompt intent did not retain exact standard prompt hash")
				}
				for _, text := range []string{op.ID, op.Preparation.Nonce, "unansweredQ", "nextD", "lastACK", "native-source", "ao orchestrator replacement prepared"} {
					if !strings.Contains(f.runtime.prompts[0], text) {
						t.Fatalf("standard prompt missing %q", text)
					}
				}
			}
		})
	}
}

func TestReplacementIdleDeadlineIsDurableAndRetryable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	svc := replacement.New(replacement.Deps{Store: f.store, Runtime: f.runtime, Documents: f.docs, Now: func() time.Time { return now }})
	op, err := svc.Accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	f.runtime.observation.State = ports.ChatControllerBusy
	for range 3 {
		op, err = svc.Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	now = op.DeadlineAt.Add(time.Second)
	op, err = svc.Advance(ctx, op.ID)
	if err != nil || op.Phase != domain.ReplacementAwaitingSource || op.ResumePhase != domain.ReplacementWaitingSourceIdle || op.Error == nil || !op.Error.Retryable {
		t.Fatalf("deadline lost visible retry state: %+v %v", op, err)
	}
	got, err := svc.Get(ctx, op.ID)
	if err != nil || got.Phase != op.Phase || got.Error.Code != op.Error.Code {
		t.Fatal("deadline not durable", err)
	}
}

func TestReplacementNativeObservationWaitIsBoundedAndNeverResends(t *testing.T) {
	for _, phase := range []domain.ReplacementPhase{domain.ReplacementWaitingSourceIdle, domain.ReplacementPreparingHandoff} {
		for _, nativeCode := range []string{"TRANSCRIPT_TAIL_UNSETTLED", "TRANSCRIPT_COVERAGE_INCOMPLETE", "TRANSCRIPT_CUTOFF_INVALID", "TRANSCRIPT_SCHEMA_UNSUPPORTED"} {
			t.Run(string(phase)+"/"+nativeCode, func(t *testing.T) {
				f := setup(t)
				ctx := context.Background()
				now := time.Now().UTC()
				documents := projectdocuments.New(projectdocuments.Deps{Store: f.store, DataDir: t.TempDir(), Capabilities: sourceCapability{}})
				svc := replacement.New(replacement.Deps{Store: f.store, Runtime: f.runtime, Documents: documents, Now: func() time.Time { return now }})
				op, err := svc.Accept(ctx, f.request)
				if err != nil {
					t.Fatal(err)
				}
				for range 8 {
					if op.Phase == phase {
						break
					}
					op, err = svc.Advance(ctx, op.ID)
					if err != nil || op.Error != nil {
						t.Fatal(err, op.Error)
					}
				}
				if op.Phase != phase {
					t.Fatal("fixture never reached observation stage")
				}
				f.runtime.observationErr = &ports.NativeTranscriptError{Code: nativeCode, Reason: "native fixture not verified"}
				for range 2 {
					op, err = svc.Advance(ctx, op.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				want := phase
				if nativeCode == "TRANSCRIPT_SCHEMA_UNSUPPORTED" {
					want = domain.ReplacementAwaitingSource
					if phase == domain.ReplacementPreparingHandoff {
						want = domain.ReplacementAwaitingHandoff
					}
				}
				if op.Phase != want || op.Error == nil || op.Error.Code != nativeCode || f.runtime.preparations != 0 || f.runtime.quiescences != 0 {
					t.Fatalf("observation phase=%s want=%s failure=%+v sends=%d stops=%d", op.Phase, want, op.Error, f.runtime.preparations, f.runtime.quiescences)
				}
				if want != phase {
					return
				}
				now = op.DeadlineAt.Add(time.Second)
				op, err = svc.Advance(ctx, op.ID)
				if err != nil || op.Phase == phase || op.Error == nil || f.runtime.preparations != 0 || f.runtime.quiescences != 0 {
					t.Fatalf("unbounded native wait: phase=%s error=%+v err=%v", op.Phase, op.Error, err)
				}
			})
		}
	}
}

func TestReplacementObservationSourceRotationKeepsNamedSourceWait(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	op, err := f.svc.Accept(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		op, err = f.svc.Advance(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	f.runtime.observationErr = domain.ErrReplacementSourceChanged
	op, err = f.svc.Advance(ctx, op.ID)
	if err != nil || op.Phase != domain.ReplacementAwaitingSource || op.Error == nil || op.Error.Code != "ORCHESTRATOR_REPLACEMENT_SOURCE_CHANGED" {
		t.Fatalf("source rotation lost named wait: phase=%s error=%+v err=%v", op.Phase, op.Error, err)
	}
}
