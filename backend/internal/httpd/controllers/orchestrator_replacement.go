package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
	docs "github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
)

// OrchestratorReplacementService is the short-lived API boundary. Provider work
// belongs to daemon reconciliation, never to a caller waiting on its own stop.
type OrchestratorReplacementService interface {
	Accept(context.Context, replacement.Request) (domain.OrchestratorReplacement, error)
	Get(context.Context, string) (domain.OrchestratorReplacement, error)
	Intake(context.Context, string) (domain.ReplacementIntake, error)
	Retry(context.Context, string) (domain.OrchestratorReplacement, error)
	Cancel(context.Context, string) (domain.OrchestratorReplacement, error)
	Prepared(context.Context, string, docs.ReplacementPreparationInput) (domain.OrchestratorReplacement, error)
}

// OrchestratorReplacementController serves durable handoff receipts and reads.
type OrchestratorReplacementController struct {
	Svc OrchestratorReplacementService
}

// Register mounts the public operation boundary.
func (c *OrchestratorReplacementController) Register(r chi.Router) {
	r.Post("/projects/{id}/orchestrator/replacements", c.accept)
	r.Get("/orchestrator/replacements/{operationId}", c.get)
	r.Get("/orchestrator/replacements/{operationId}/intake", c.intake)
	r.Post("/orchestrator/replacements/{operationId}/retry", c.retry)
	r.Post("/orchestrator/replacements/{operationId}/cancel", c.cancel)
	r.Post("/orchestrator/replacements/{operationId}/prepared", c.prepared)
	r.Get("/orchestrator/replacements/{operationId}/diagnose", c.diagnose)
	r.Post("/orchestrator/replacements/{operationId}/repair", c.repair)
}

func (c *OrchestratorReplacementController) available(w http.ResponseWriter, r *http.Request, path string) bool {
	if c.Svc != nil {
		return true
	}
	apispec.NotImplemented(w, r, r.Method, "/api/v1"+path)
	return false
}

func (c *OrchestratorReplacementController) accept(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/projects/{id}/orchestrator/replacements") {
		return
	}
	var in OrchestratorReplaceRequest
	if err := decodeReplacementRequest(r, &in, false); err != nil {
		replacementBadRequest(w, r)
		return
	}
	op, err := c.Svc.Accept(r.Context(), replacement.Request{ProjectID: domain.ProjectID(chi.URLParam(r, "id")), SourceSessionID: in.SourceSessionID, OperationKey: in.OperationKey})
	c.write(w, r, op, err, http.StatusAccepted)
}

func replacementOperationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "operationId")
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		replacementBadRequest(w, r)
		return "", false
	}
	return id, true
}
func replacementBadRequest(w http.ResponseWriter, r *http.Request) {
	envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "INVALID_REPLACEMENT_REQUEST", "A canonical operation ID and valid replacement request are required", nil)
}

func (c *OrchestratorReplacementController) get(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/orchestrator/replacements/{operationId}") {
		return
	}
	id, ok := replacementOperationID(w, r)
	if !ok {
		return
	}
	op, err := c.Svc.Get(r.Context(), id)
	c.write(w, r, op, err, http.StatusOK)
}
func (c *OrchestratorReplacementController) retry(w http.ResponseWriter, r *http.Request) {
	c.action(w, r, "retry")
}

func (c *OrchestratorReplacementController) intake(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/orchestrator/replacements/{operationId}/intake") {
		return
	}
	id, ok := replacementOperationID(w, r)
	if !ok {
		return
	}
	in, err := c.Svc.Intake(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if in.OperationID != id || in.ProjectID == "" || in.SourceSessionID == "" {
		envelope.WriteError(w, r, errors.New("replacement service returned an invalid intake inventory"))
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OrchestratorReplacementIntakeResponse{Intake: in})
}
func (c *OrchestratorReplacementController) cancel(w http.ResponseWriter, r *http.Request) {
	c.action(w, r, "cancel")
}

func (c *OrchestratorReplacementController) action(w http.ResponseWriter, r *http.Request, name string) {
	if !c.available(w, r, "/orchestrator/replacements/{operationId}/"+name) {
		return
	}
	id, ok := replacementOperationID(w, r)
	if !ok {
		return
	}
	var empty OrchestratorReplacementActionRequest
	if err := decodeReplacementRequest(r, &empty, true); err != nil {
		replacementBadRequest(w, r)
		return
	}
	var op domain.OrchestratorReplacement
	var err error
	if name == "retry" {
		op, err = c.Svc.Retry(r.Context(), id)
	} else {
		op, err = c.Svc.Cancel(r.Context(), id)
	}
	c.write(w, r, op, err, http.StatusAccepted)
}
func (c *OrchestratorReplacementController) prepared(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "/orchestrator/replacements/{operationId}/prepared") {
		return
	}
	id, ok := replacementOperationID(w, r)
	if !ok {
		return
	}
	var in OrchestratorReplacementPreparedRequest
	if err := decodeReplacementRequest(r, &in, false); err != nil {
		replacementBadRequest(w, r)
		return
	}
	op, err := c.Svc.Prepared(r.Context(), id, docs.ReplacementPreparationInput(in))
	c.write(w, r, op, err, http.StatusAccepted)
}
func (c *OrchestratorReplacementController) write(w http.ResponseWriter, r *http.Request, op domain.OrchestratorReplacement, err error, status int) {
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	id, parseErr := uuid.Parse(op.ID)
	if parseErr != nil || id == uuid.Nil || id.String() != op.ID || !op.Phase.Valid() || op.ProjectID == "" || op.SourceSessionID == "" {
		envelope.WriteError(w, r, errors.New("replacement service returned an invalid durable receipt"))
		return
	}
	envelope.WriteJSON(w, status, OrchestratorReplacementResponse{Operation: replacementPublicView(op)})
}

func replacementPublicView(op domain.OrchestratorReplacement) OrchestratorReplacementView {
	view := OrchestratorReplacementView{ID: op.ID, OperationKey: op.OperationKey, ProjectID: op.ProjectID, SourceSessionID: op.SourceSessionID, SuccessorSessionID: op.SuccessorSessionID, Phase: op.Phase, ResumePhase: op.ResumePhase, CreatedAt: op.CreatedAt, UpdatedAt: op.UpdatedAt, DeadlineAt: op.DeadlineAt, SourceStopIntentAt: op.SourceStopIntentAt, SourceRetiredAt: op.SourceRetiredAt, FrozenAt: op.FrozenAt, IntakeReleasedAt: op.IntakeReleasedAt, IntakeDrainRequestedAt: op.IntakeDrainRequestedAt, IntakeTransfer: op.IntakeTransfer, SourceIntakeReleasedAt: op.SourceIntakeReleasedAt, SharedDir: op.SharedDir, DocumentPath: op.DocumentPath, Preparation: replacementPreparationView(op.Preparation), Evidence: replacementEvidenceView(op.Preparation, op.Transcript), NotificationReceipts: make([]ReplacementNotificationView, 0, len(op.Notifications)), Attempts: make([]ReplacementAttemptView, 0, len(op.Attempts)), Error: replacementFailureView(op.Error), ReadCommand: "ao orchestrator replacement get " + op.ID + " --json"}
	view.Revision, view.IntakeRevision = op.Revision, op.IntakeRevision
	view.ControlAttempts = make([]ReplacementControlAttemptView, 0, len(op.ControlAttempts))
	for _, attempt := range op.ControlAttempts {
		item := ReplacementControlAttemptView{Kind: attempt.Kind, ClientID: attempt.ClientID, State: attempt.State, ArchivedAt: attempt.ArchivedAt}
		if attempt.Bootstrap != nil {
			item.IntentAt, item.TurnID, item.SettledAt = attempt.Bootstrap.IntentAt, attempt.Bootstrap.TurnID, attempt.Bootstrap.SettledAt
		}
		if attempt.Probe != nil {
			item.IntentAt, item.TurnID, item.SettledAt = attempt.Probe.IntentAt, attempt.Probe.TurnID, attempt.Probe.SettledAt
		}
		view.ControlAttempts = append(view.ControlAttempts, item)
	}
	if retry := op.RetainedRetry; retry != nil {
		view.RetainedRetry = &ReplacementRetainedRetryView{Phase: retry.Phase, Failure: *replacementFailureView(&retry.Failure), RequestedAt: retry.RequestedAt}
	}
	if op.Phase == domain.ReplacementRecoveryRequired {
		view.OperatorGuidance = "Preserve this operation and its immutable evidence. Read the durable facts with readCommand; retryCommand only reconciles the retained boundary and never authorizes a fresh replacement. Do not edit the database, revive the retired source, or clear an uncertain intent. An operator must verify the native runtime before recovery can continue."
		if p := op.ResumeEvidence; p != nil && p.ResumeIntentAt != nil && p.ResumedAt == nil {
			view.OperatorGuidance += " The resume intent has no launch confirmation. Retry checks the persisted pre-launch capability boundary; an unchanged strongly stopped owner can record no-launch evidence. Otherwise it only looks up the reserved owner or reconnects an existing host with strict same-native replay, never starts a provider. Unknown evidence retains the fence."
		}
	}
	if !op.Phase.Terminal() {
		view.RetryCommand = "ao orchestrator replacement retry " + op.ID
		if op.SourceStopIntentAt == nil && op.SourceStop == nil && op.SourceRetiredAt == nil {
			view.CancelCommand = "ao orchestrator replacement cancel " + op.ID
		}
	}
	if op.SourceStop != nil {
		view.SourceStoppedAt = &op.SourceStop.ConfirmedAt
	}
	if b := op.Bootstrap; b != nil {
		view.ResumeEvidence.BootstrapIntentAt = &b.IntentAt
		view.ResumeEvidence.BootstrapTurnID = b.TurnID
		view.ResumeEvidence.BootstrapSettledAt = b.SettledAt
	}
	if p := op.ResumeEvidence; p != nil {
		view.ResumeEvidence.StartedAt = p.StartedAt
		view.ResumeEvidence.ResumeIntentAt, view.ResumeEvidence.ResumeGeneration = p.ResumeIntentAt, p.ResumeGeneration
		view.ResumeEvidence.UnlaunchedAttempts = append([]domain.ReplacementResumeUnlaunched{}, p.UnlaunchedAttempts...)
		if p.StopReceipt != nil {
			view.ResumeEvidence.StopReceipt = &ReplacementStopView{NativeID: p.StopReceipt.NativeID, Method: p.StopReceipt.Method, ConfirmedAt: p.StopReceipt.ConfirmedAt}
		}
		view.ResumeEvidence.BeforeNativeID, view.ResumeEvidence.AfterNativeID = p.BeforeNativeID, p.AfterNativeID
		view.ResumeEvidence.BeforeGeneration, view.ResumeEvidence.AfterGeneration = p.BeforeOwner.ControllerGeneration, p.AfterOwner.ControllerGeneration
		view.ResumeEvidence.StopConfirmed, view.ResumeEvidence.HistoryVerified = p.StopConfirmed, p.HistoryVerified
		view.ResumeEvidence.ResumedAt, view.ResumeEvidence.VerifiedAt = p.ResumedAt, p.VerifiedAt
		if p.Probe != nil {
			view.ResumeEvidence.ProbeIntentAt = &p.Probe.IntentAt
			view.ResumeEvidence.ProbeTurnID = p.Probe.TurnID
			view.ResumeEvidence.ProbeSettledAt = p.Probe.SettledAt
		}
		view.ResumeEvidence.Verified = p.VerifiedAt != nil && p.StopConfirmed && p.HistoryVerified && p.BeforeNativeID != "" && p.AfterNativeID == p.BeforeNativeID && p.BeforeOwner.ControllerGeneration != "" && p.AfterOwner.ControllerGeneration != "" && p.BeforeOwner.ControllerGeneration != p.AfterOwner.ControllerGeneration && p.Probe != nil && p.Probe.SettledAt != nil
	}
	if w := op.WriterActivation; w != nil {
		view.WriterActivation = &ReplacementWriterView{SessionID: op.SuccessorSessionID, BeforeEpoch: w.BeforeEpoch, AfterEpoch: w.AfterEpoch, ActivatedAt: w.ActivatedAt}
	}
	for _, notice := range op.Notifications {
		view.NotificationReceipts = append(view.NotificationReceipts, ReplacementNotificationView{SessionID: notice.SessionID, ContactID: notice.ContactID, State: notice.State, CreatedAt: notice.CreatedAt})
	}
	for _, attempt := range op.Attempts {
		view.Attempts = append(view.Attempts, ReplacementAttemptView{ID: attempt.ID, Phase: attempt.Phase, SourceIntakeReleasedAt: attempt.SourceIntakeReleasedAt, EndedAt: attempt.EndedAt, FrozenAt: attempt.FrozenAt, Preparation: replacementPreparationView(attempt.Preparation), Evidence: replacementEvidenceView(attempt.Preparation, attempt.Transcript), Error: replacementFailureView(attempt.Error)})
	}
	view.ProvisioningAttempts = make([]ReplacementProvisioningAttemptView, 0, len(op.SpawnAttempts))
	for i, attempt := range op.SpawnAttempts {
		view.ProvisioningAttempts = append(view.ProvisioningAttempts, ReplacementProvisioningAttemptView{Number: i + 1, ReservedAt: attempt.StartedAt, LaunchIntentAt: attempt.LaunchIntentAt, FailedAt: attempt.FailedAt})
	}
	return view
}

func replacementFailureView(failure *domain.ReplacementFailure) *domain.ReplacementFailure {
	if failure == nil {
		return nil
	}
	return &domain.ReplacementFailure{Code: failure.Code, Retryable: failure.Retryable, Message: "Inspect the operation's durable phase and evidence; available controls are listed separately."}
}
func replacementPreparationView(p *domain.ReplacementPreparation) *ReplacementPreparationView {
	if p == nil {
		return nil
	}
	return &ReplacementPreparationView{IntakeRevision: p.IntakeRevision, PromptIntentAt: p.PromptIntentAt, StartedAt: p.StartedAt, DeadlineAt: p.DeadlineAt, AttemptID: p.AttemptID, LeaseEpoch: p.LeaseEpoch, ControlTurnID: p.ControlTurnID, SubmittedHash: p.SubmittedHash, WriteReceiptID: p.WriteReceiptID, SubmittedAt: p.SubmittedAt, VerifiedAt: p.VerifiedAt}
}
func replacementEvidenceView(p *domain.ReplacementPreparation, t *domain.ReplacementTranscript) *ReplacementEvidenceView {
	if t == nil {
		return nil
	}
	view := &ReplacementEvidenceView{AttemptID: t.AttemptID, TranscriptHash: t.TranscriptHash, CapturedAt: t.CapturedAt, ConversationCutoffs: t.AO.ConversationCutoffs, NativeCutoffs: append([]domain.TranscriptEpochCutoff{}, t.NativeCutoffs...)}
	if p != nil {
		view.HandoffHash = p.SubmittedHash
	}
	if r := t.Receipt; r != nil {
		view.Directory, view.ManifestHash = r.Directory, r.ManifestHash
		view.HandoffPath = filepath.Join(r.Directory, "handoff.md")
		view.TranscriptPath = filepath.Join(r.Directory, "transcript.md")
		view.ManifestPath = filepath.Join(r.Directory, "manifest.json")
	}
	return view
}

// Replacement control bodies contain identifiers and receipts, never draft or
// transcript content. Reject trailing values and null before service mutation.
func decodeReplacementRequest(r *http.Request, out any, allowEmpty bool) error {
	const limit = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return errors.New("replacement request exceeds control-body limit")
	}
	// Retry/cancel have no requestBody in the public schema. A zero-byte body
	// is their empty argument object; supplied bodies still require strict JSON.
	if allowEmpty && len(raw) == 0 {
		return nil
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("replacement request requires a bounded JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("replacement request has trailing data")
	}
	return nil
}
