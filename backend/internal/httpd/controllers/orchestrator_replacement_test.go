package controllers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const replacementAPIID = "11111111-1111-4111-8111-111111111111"

func TestReplacementHTTPRecoveryFactsAndSafeOperatorGuidance(t *testing.T) {
	op := replacementAPIState()
	op.Revision, op.IntakeRevision = 42, 12
	op.Preparation.IntakeRevision = 11
	op.Preparation.PromptIntentAt = &op.UpdatedAt
	op.Phase, op.ResumePhase = domain.ReplacementRecoveryRequired, domain.ReplacementVerifyingResume
	op.Error = &domain.ReplacementFailure{Code: "ORCHESTRATOR_HANDOFF_RESUME_INCONCLUSIVE", Message: "private-provider-error", Retryable: true}
	op.ResumeEvidence.ResumeIntentAt = &op.UpdatedAt
	op.ResumeEvidence.ResumeGeneration = "resume-generation"
	op.ResumeEvidence.UnlaunchedAttempts = []domain.ReplacementResumeUnlaunched{{Generation: "unlaunched-generation", IntentAt: op.CreatedAt, ConfirmedAt: op.UpdatedAt}}
	op.ControlAttempts = []domain.ReplacementControlAttempt{{Kind: "probe", ClientID: "control-client", State: domain.TurnStateFailed, ArchivedAt: op.UpdatedAt, Probe: &domain.ReplacementContinuityProbe{Nonce: "private-control-nonce", PromptHash: "private-control-hash", TurnID: "failed-probe", IntentAt: op.CreatedAt}}}
	op.RetainedRetry = &domain.ReplacementRetainedRetry{Phase: domain.ReplacementVerifyingResume, FactsSHA256: "private-retry-hash", Failure: domain.ReplacementFailure{Code: "ORIGINAL_RETRY_CAUSE", Message: "private-original-error", Retryable: true}, RequestedAt: op.UpdatedAt}
	router := chi.NewRouter()
	(&controllers.OrchestratorReplacementController{Svc: &replacementAPI{op: op}}).Register(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/orchestrator/replacements/"+op.ID, nil))
	for _, required := range []string{`"revision":42`, `"intakeRevision":12`, `"intakeRevision":11`, "promptIntentAt", "bootstrapIntentAt", "controlAttempts", "failed-probe", "unlaunchedAttempts", "unlaunched-generation", "resumeIntentAt", "resume-generation", "retainedRetry", "ORIGINAL_RETRY_CAUSE", "operatorGuidance", "no launch"} {
		if !strings.Contains(w.Body.String(), required) {
			t.Errorf("missing safe recovery fact %q", required)
		}
	}
	for _, private := range []string{"private-provider-error", "private-control-nonce", "private-control-hash", "private-retry-hash", "private-original-error", "successor-secret", "before-secret"} {
		if strings.Contains(w.Body.String(), private) {
			t.Errorf("leaked private recovery field %q", private)
		}
	}
}

type replacementAPI struct {
	op       domain.OrchestratorReplacement
	err      error
	calls    []string
	request  replacement.Request
	prepared docs.ReplacementPreparationInput
}

func (s *replacementAPI) Accept(_ context.Context, in replacement.Request) (domain.OrchestratorReplacement, error) {
	s.calls = append(s.calls, "accept")
	s.request = in
	return s.op, s.err
}
func (s *replacementAPI) Get(_ context.Context, id string) (domain.OrchestratorReplacement, error) {
	s.calls = append(s.calls, "get:"+id)
	return s.op, s.err
}
func (s *replacementAPI) Retry(_ context.Context, id string) (domain.OrchestratorReplacement, error) {
	s.calls = append(s.calls, "retry:"+id)
	return s.op, s.err
}
func (s *replacementAPI) Cancel(_ context.Context, id string) (domain.OrchestratorReplacement, error) {
	s.calls = append(s.calls, "cancel:"+id)
	return s.op, s.err
}
func (s *replacementAPI) Prepared(_ context.Context, id string, in docs.ReplacementPreparationInput) (domain.OrchestratorReplacement, error) {
	s.calls = append(s.calls, "prepared:"+id)
	s.prepared = in
	return s.op, s.err
}

func replacementAPIState() domain.OrchestratorReplacement {
	now := time.Now().UTC()
	return domain.OrchestratorReplacement{ID: replacementAPIID, OperationKey: replacementAPIID, ProjectID: "demo", SourceSessionID: "source", SuccessorSessionID: "successor", Phase: domain.ReplacementNotifying, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour), FrozenAt: &now, SourceRetiredAt: &now, SharedDir: "/shared/project", DocumentPath: "handoff/HANDOFF.md", SourceCapabilityVerifier: "source-secret", TargetAccountID: "private-account", TargetAgentConfig: domain.AgentConfig{Model: "private-config"},
		SpawnAttempts:       []domain.ReplacementSpawnAttempt{{ID: "private-spawn-attempt", StartedAt: now, LaunchIntentAt: &now}},
		Preparation:         &domain.ReplacementPreparation{AttemptID: "prepare-attempt", Nonce: "private-nonce", ScratchDir: "private-scratch", SubmittedHash: "handoff-sha", WriteReceiptID: "write-receipt", ControlTurnID: "prepare-turn", VerifiedAt: &now},
		Transcript:          &domain.ReplacementTranscript{AttemptID: "export-attempt", TranscriptHash: "transcript-sha", Receipt: &domain.HandoffEvidenceReceipt{Directory: "/immutable/evidence", ManifestHash: "manifest-sha", InputHash: "private-input-binding"}, NativeCutoffs: []domain.TranscriptEpochCutoff{{NativeID: "source-native", SHA256: "native-sha", Schema: "claude-jsonl", Bytes: 100, LeafID: "source-leaf"}}, AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{"conversation": 42}}},
		SuccessorController: &domain.ReplacementSuccessorController{CapabilityVerifier: "successor-secret"},
		Bootstrap:           &domain.ReplacementBootstrap{PromptHash: "private-prompt-hash", TurnID: "bootstrap-turn", IntentAt: now, SettledAt: &now},
		ResumeEvidence:      &domain.ReplacementResumeEvidence{BeforeNativeID: "successor-native", AfterNativeID: "successor-native", BeforeOwner: domain.SessionControllerOwner{ControllerGeneration: "before-generation"}, AfterOwner: domain.SessionControllerOwner{ControllerGeneration: "after-generation"}, BeforeCapabilityVerifier: "before-secret", AfterCapabilityVerifier: "after-secret", StopConfirmed: true, HistoryVerified: true, VerifiedAt: &now, Probe: &domain.ReplacementContinuityProbe{Nonce: "private-probe-nonce", TurnID: "probe-turn", SettledAt: &now}},
		WriterActivation:    &domain.ReplacementWriterActivation{BeforeEpoch: 3, AfterEpoch: 4, CapabilityVerifier: "writer-secret", ActivatedAt: now}, Notifications: []domain.ReplacementWorkerNotification{{SessionID: "worker", ContactID: "handoff-notice", State: "pending", CreatedAt: now}}, Error: &domain.ReplacementFailure{Code: "WAITING_NOTIFICATIONS", Message: "pending worker delivery", Retryable: true},
	}
}

func TestReplacementHTTPReceiptsExposeSafeDurableFacts(t *testing.T) {
	for _, action := range []string{"accept", "get", "retry", "cancel", "prepared"} {
		t.Run(action, func(t *testing.T) {
			svc := &replacementAPI{op: replacementAPIState()}
			router := chi.NewRouter()
			(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
			path := "/orchestrator/replacements/" + replacementAPIID
			method := http.MethodPost
			body := `{}`
			expectedCall := action + ":" + replacementAPIID
			switch action {
			case "accept":
				path = "/projects/demo/orchestrator/replacements"
				body = `{"sourceSessionId":"source","operationKey":"` + replacementAPIID + `"}`
				expectedCall = "accept"
			case "get":
				method = http.MethodGet
			case "prepared":
				path += "/prepared"
				body = `{"actor":{"sessionId":"source","capability":"request-secret"},"attemptId":"attempt","nonce":"nonce","path":"handoff/HANDOFF.md","hash":"handoff-sha","writeReceiptId":"receipt"}`
			default:
				path += "/" + action
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
			want := http.StatusAccepted
			if action == "get" {
				want = http.StatusOK
			}
			if w.Code != want || len(svc.calls) != 1 || svc.calls[0] != expectedCall {
				t.Fatalf("receipt status=%d calls=%v body=%s", w.Code, svc.calls, w.Body.String())
			}
			for _, secret := range []string{"source-secret", "successor-secret", "before-secret", "after-secret", "writer-secret", "request-secret", "private-account", "private-config", "private-nonce", "private-scratch", "private-probe-nonce", "private-prompt-hash", "private-input-binding", "capabilityVerifier", "private-spawn-attempt"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatalf("internal state leaked: %s", secret)
				}
			}
			var response struct {
				Operation struct {
					ID        string `json:"id"`
					Phase     string `json:"phase"`
					SharedDir string `json:"sharedDir"`
					Evidence  struct {
						TranscriptHash string            `json:"transcriptHash"`
						ManifestHash   string            `json:"manifestHash"`
						HandoffHash    string            `json:"handoffHash"`
						NativeCutoffs  []json.RawMessage `json:"nativeCutoffs"`
					} `json:"evidence"`
					ResumeEvidence struct {
						Verified       bool   `json:"verified"`
						BeforeNativeID string `json:"beforeNativeId"`
						AfterNativeID  string `json:"afterNativeId"`
						ProbeTurnID    string `json:"probeTurnId"`
					} `json:"resumeEvidence"`
					ProvisioningAttempts []controllers.ReplacementProvisioningAttemptView `json:"provisioningAttempts"`
					NotificationReceipts []json.RawMessage                                `json:"notificationReceipts"`
					Error                *domain.ReplacementFailure                       `json:"error"`
					RetryCommand         string                                           `json:"retryCommand"`
				} `json:"operation"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			op := response.Operation
			if len(op.ProvisioningAttempts) != 1 || op.ProvisioningAttempts[0].Number != 1 || op.ProvisioningAttempts[0].ReservedAt.IsZero() || op.ProvisioningAttempts[0].LaunchIntentAt == nil {
				t.Fatal("safe provisioning receipt lost", w.Body.String())
			}

			if op.ID != replacementAPIID || op.Phase != "notifying" || op.SharedDir != "/shared/project" || op.Evidence.TranscriptHash != "transcript-sha" || op.Evidence.ManifestHash != "manifest-sha" || op.Evidence.HandoffHash != "handoff-sha" || len(op.Evidence.NativeCutoffs) != 1 || !op.ResumeEvidence.Verified || op.ResumeEvidence.BeforeNativeID != "successor-native" || op.ResumeEvidence.AfterNativeID != "successor-native" || op.ResumeEvidence.ProbeTurnID != "probe-turn" || len(op.NotificationReceipts) != 1 || op.Error == nil || op.RetryCommand != "ao orchestrator replacement retry "+replacementAPIID {
				t.Fatal("durable facts lost", w.Body.String())
			}
			if action == "accept" && (svc.request.ProjectID != "demo" || svc.request.SourceSessionID != "source" || svc.request.OperationKey != replacementAPIID) {
				t.Fatal("acceptance intent changed", svc.request)
			}
			if action == "prepared" && (svc.prepared.Actor.Capability != "request-secret" || svc.prepared.WriteReceiptID != "receipt") {
				t.Fatal("authenticated receipt lost")
			}
		})
	}
}

func TestReplacementHTTPActionsAllowNoBody(t *testing.T) {
	for _, action := range []string{"retry", "cancel"} {
		for _, transport := range []string{"nil", "empty", "unknown-length"} {
			t.Run(action+"/"+transport, func(t *testing.T) {
				svc := &replacementAPI{op: replacementAPIState()}
				router := chi.NewRouter()
				(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
				request := httptest.NewRequest(http.MethodPost, "/orchestrator/replacements/"+replacementAPIID+"/"+action, nil)
				if transport != "nil" {
					request = httptest.NewRequest(http.MethodPost, request.URL.String(), strings.NewReader(""))
				}
				if transport == "unknown-length" {
					request.ContentLength = -1
				}
				w := httptest.NewRecorder()
				router.ServeHTTP(w, request)
				if w.Code != http.StatusAccepted || len(svc.calls) != 1 || svc.calls[0] != action+":"+replacementAPIID {
					t.Fatal("body-free action did not reach service exactly once", w.Code, svc.calls, w.Body.String())
				}
			})
		}
	}
}

func TestReplacementHTTPActionsStillRejectInvalidBodies(t *testing.T) {
	for _, action := range []string{"retry", "cancel"} {
		for name, body := range map[string]string{
			"unknown-field":   `{"fresh":true}`,
			"trailing":        `{} {}`,
			"null":            `null`,
			"array":           `[]`,
			"whitespace":      ` `,
			"oversized":       `{}` + strings.Repeat(" ", 64<<10),
			"oversized-blank": strings.Repeat(" ", (64<<10)+1),
		} {
			t.Run(action+"/"+name, func(t *testing.T) {
				svc := &replacementAPI{op: replacementAPIState()}
				router := chi.NewRouter()
				(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/orchestrator/replacements/"+replacementAPIID+"/"+action, strings.NewReader(body)))
				if w.Code != http.StatusBadRequest || len(svc.calls) != 0 {
					t.Fatal("invalid action body reached service", w.Code, svc.calls, w.Body.String())
				}
			})
		}
	}
}

func TestReplacementHTTPRejectsInvalidBodiesBeforeService(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/projects/demo/orchestrator/replacements", `{"sourceSessionId":"source","operationKey":"` + replacementAPIID + `","harness":"codex"}`},
		{"POST", "/orchestrator/replacements/" + replacementAPIID + "/retry", `{"fresh":true}`},
		{"POST", "/orchestrator/replacements/" + replacementAPIID + "/cancel", `{} {}`},
		{"POST", "/orchestrator/replacements/" + replacementAPIID + "/prepared", `{"sourceOwner":"forged"}`},
		{"GET", "/orchestrator/replacements/not-a-uuid", ``},
		{"POST", "/orchestrator/replacements/" + replacementAPIID + "/cancel", `null`},
		{"POST", "/orchestrator/replacements/" + replacementAPIID + "/cancel", `{}` + strings.Repeat(" ", 64<<10)},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			svc := &replacementAPI{op: replacementAPIState()}
			router := chi.NewRouter()
			(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if w.Code != http.StatusBadRequest || len(svc.calls) != 0 {
				t.Fatal("invalid request reached service", w.Code, svc.calls, w.Body.String())
			}
		})
	}
}

func TestReplacementHTTPPreservesNamedErrorAndDoesNotReadAfterAccept(t *testing.T) {
	svc := &replacementAPI{err: apierr.Conflict("ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED", "Claude Code is required", nil)}
	router := chi.NewRouter()
	(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/projects/demo/orchestrator/replacements", strings.NewReader(`{"sourceSessionId":"source","operationKey":"`+replacementAPIID+`"}`)))
	if w.Code != http.StatusConflict || len(svc.calls) != 1 || !strings.Contains(w.Body.String(), "ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED") {
		t.Fatal("named error lost", w.Code, svc.calls, w.Body.String())
	}
}

func TestReplacementHTTPReadSurvivesSQLiteReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlitetest.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	now := time.Now().UTC()
	if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "read-replacement", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	source, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "read-replacement", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{ControllerGeneration: "source-generation", ProviderConversationID: "source-native", BrowserCapabilityVerifier: "persisted-private-verifier"}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	op := domain.OrchestratorReplacement{SchemaVersion: 1, ID: replacementAPIID, OperationKey: replacementAPIID, RequestHash: strings.Repeat("a", 64), ProjectID: source.ProjectID, SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)}
	if _, created, err := db.CreateOrchestratorReplacement(ctx, op); err != nil || !created {
		t.Fatal(created, err)
	}
	read := func() string {
		t.Helper()
		router := chi.NewRouter()
		(&controllers.OrchestratorReplacementController{Svc: replacement.New(replacement.Deps{Store: db})}).Register(router)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/orchestrator/replacements/"+op.ID, nil))
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "persisted-private-verifier") {
			t.Fatal("persisted read failed", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	first := read()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db, err = sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again := read(); again != first {
		t.Fatal("reopened operation lost durable facts", again)
	}
}

func TestReplacementHTTPAttemptAndResumeViewsAreExplicit(t *testing.T) {
	for _, change := range []string{"verified", "no probe", "different native", "same generation", "no history", "no stop"} {
		t.Run(change, func(t *testing.T) {
			op := replacementAPIState()
			op.Attempts = []domain.ReplacementAttempt{{ID: "prior-attempt", Phase: domain.ReplacementAwaitingTranscript, Preparation: op.Preparation, Transcript: op.Transcript, EndedAt: op.UpdatedAt}}
			switch change {
			case "no probe":
				op.ResumeEvidence.Probe = nil
			case "different native":
				op.ResumeEvidence.AfterNativeID = "unrelated-native"
			case "same generation":
				op.ResumeEvidence.AfterOwner.ControllerGeneration = op.ResumeEvidence.BeforeOwner.ControllerGeneration
			case "no history":
				op.ResumeEvidence.HistoryVerified = false
			case "no stop":
				op.ResumeEvidence.StopConfirmed = false
			}
			svc := &replacementAPI{op: op}
			router := chi.NewRouter()
			(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/orchestrator/replacements/"+op.ID, nil))
			var got struct {
				Operation struct {
					ResumeEvidence struct {
						Verified bool `json:"verified"`
					} `json:"resumeEvidence"`
					Attempts []struct {
						ID       string `json:"id"`
						Evidence struct {
							TranscriptHash string `json:"transcriptHash"`
						} `json:"evidence"`
					} `json:"attempts"`
				} `json:"operation"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || got.Operation.ResumeEvidence.Verified != (change == "verified") || len(got.Operation.Attempts) != 1 || got.Operation.Attempts[0].ID != "prior-attempt" || got.Operation.Attempts[0].Evidence.TranscriptHash != "transcript-sha" {
				t.Fatal("unsafe or missing proof facts", w.Code, w.Body.String())
			}
			for _, secret := range []string{"private-nonce", "private-scratch", "private-input-binding"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatal("archive leaked internal state", secret)
				}
			}
		})
	}
}

func (s *replacementAPI) Intake(_ context.Context, id string) (domain.ReplacementIntake, error) {
	s.calls = append(s.calls, "intake:"+id)
	return domain.ReplacementIntake{OperationID: id, ProjectID: "demo", SourceSessionID: "source", Phase: domain.ReplacementPreparingHandoff, Messages: []domain.ReplacementIntakeMessage{{ID: "queued-A-Q22", Text: "A-Q22: B", DeliveryContentJSON: `[{"type":"image","reference":"keep.png"}]`, NativeDelivery: "unconfirmed"}}, Reports: []domain.ReplacementIntakeReport{{ID: "open-Q23", Note: "Q23: boundary?", DeliveryState: domain.ReportPending}}}, s.err
}
func TestReplacementHTTPPendingIntakeOnlyReads(t *testing.T) {
	svc := &replacementAPI{op: replacementAPIState()}
	router := chi.NewRouter()
	(&controllers.OrchestratorReplacementController{Svc: svc}).Register(router)
	result := httptest.NewRecorder()
	router.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/orchestrator/replacements/"+replacementAPIID+"/intake", nil))
	if result.Code != http.StatusOK {
		t.Fatal("pending intake route unavailable", result.Code, result.Body.String())
	}
	body := result.Body.String()
	if len(svc.calls) != 1 || svc.calls[0] != "intake:"+replacementAPIID || !strings.Contains(body, "keep.png") || !strings.Contains(body, "open-Q23") || !strings.Contains(body, "unconfirmed") {
		t.Fatal("read lost payload or invoked mutation", svc.calls, body)
	}
	for _, secret := range []string{"source-secret", "private-account", "private-nonce", "before-secret", "private-input-binding"} {
		if strings.Contains(body, secret) {
			t.Fatal("intake leaked operation secret", secret)
		}
	}
}
