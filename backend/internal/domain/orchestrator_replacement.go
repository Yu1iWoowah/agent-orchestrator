package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// MaxReplacementHandoffBytes bounds the document embedded in successor input.
const MaxReplacementHandoffBytes = 128 << 10

var (
	// ErrReplacementKeyConflict rejects a reused key with different intent.
	ErrReplacementKeyConflict = errors.New("replacement operation key conflicts")
	// ErrReplacementInProgress protects the project's active replacement.
	ErrReplacementInProgress = errors.New("replacement operation is already active")
	// ErrReplacementSourceChanged rejects admission against a stale source owner.
	ErrReplacementSourceChanged = errors.New("replacement source ownership changed")
	// ErrReplacementSourceRetired prevents revival of a superseded source controller.
	ErrReplacementSourceRetired = errors.New("replacement source has permanently retired")
	// ErrReplacementSourceBusy promises no shutdown was attempted.
	ErrReplacementSourceBusy = errors.New("replacement source is not quiescent")
	// ErrReplacementConfigChanged fences settings resolved before admission.
	ErrReplacementConfigChanged = errors.New("replacement project configuration changed")
	// ErrReplacementIntakeFenced retains accepted ordinary input on its queue.
	ErrReplacementIntakeFenced = errors.New("replacement intake is fenced")
	// ErrReplacementIntakeUncertain keeps possibly dispatched input out of replay.
	ErrReplacementIntakeUncertain = errors.New("replacement intake may already have reached the provider")
	// ErrReplacementIntakeConflict protects distinct accepted input/edit receipts.
	ErrReplacementIntakeConflict = errors.New("replacement intake receipt conflicts with its target")
	// ErrReplacementStateInvalid rejects corrupt or inconsistent operation facts.
	ErrReplacementStateInvalid = errors.New("replacement operation state is invalid")
)

// ReplacementPhase records operation progress, not a session's display status.
type ReplacementPhase string

// Replacement phases distinguish progress, bounded waits and final outcomes.
const (
	ReplacementAccepted               ReplacementPhase = "accepted"
	ReplacementPreflight              ReplacementPhase = "preflight"
	ReplacementWaitingSourceIdle      ReplacementPhase = "waiting_source_idle"
	ReplacementAdoptingSharedFiles    ReplacementPhase = "adopting_shared_files"
	ReplacementPreparingHandoff       ReplacementPhase = "preparing_handoff"
	ReplacementHandoffVerified        ReplacementPhase = "handoff_verified"
	ReplacementFreezingSharedWrites   ReplacementPhase = "freezing_shared_writes"
	ReplacementExportingTranscript    ReplacementPhase = "exporting_transcript"
	ReplacementTranscriptVerified     ReplacementPhase = "transcript_verified"
	ReplacementRetiring               ReplacementPhase = "retiring"
	ReplacementStarting               ReplacementPhase = "starting"
	ReplacementControllerReady        ReplacementPhase = "controller_ready"
	ReplacementHandoffSettled         ReplacementPhase = "handoff_settled"
	ReplacementVerifyingResume        ReplacementPhase = "verifying_resume"
	ReplacementActivatingSharedWriter ReplacementPhase = "activating_shared_writer"
	ReplacementNotifying              ReplacementPhase = "notifying"
	ReplacementCompleted              ReplacementPhase = "completed"
	ReplacementAwaitingSource         ReplacementPhase = "awaiting_source"
	ReplacementAwaitingHandoff        ReplacementPhase = "awaiting_handoff"
	ReplacementAwaitingTranscript     ReplacementPhase = "awaiting_transcript"
	ReplacementRecoveryRequired       ReplacementPhase = "recovery_required"
	ReplacementCancelled              ReplacementPhase = "cancelled"
	ReplacementRejected               ReplacementPhase = "rejected"
)

// Valid prevents unknown phases from advancing a provider operation.
func (p ReplacementPhase) Valid() bool {
	switch p {
	case ReplacementAccepted, ReplacementPreflight, ReplacementWaitingSourceIdle,
		ReplacementAdoptingSharedFiles, ReplacementPreparingHandoff, ReplacementHandoffVerified,
		ReplacementFreezingSharedWrites, ReplacementExportingTranscript, ReplacementTranscriptVerified,
		ReplacementRetiring, ReplacementStarting, ReplacementControllerReady, ReplacementHandoffSettled,
		ReplacementVerifyingResume, ReplacementActivatingSharedWriter, ReplacementNotifying,
		ReplacementCompleted, ReplacementAwaitingSource, ReplacementAwaitingHandoff,
		ReplacementAwaitingTranscript, ReplacementRecoveryRequired, ReplacementCancelled, ReplacementRejected:
		return true
	}
	return false
}

// Terminal releases admission; visible waiting and recovery still reserve it.
func (p ReplacementPhase) Terminal() bool {
	return p == ReplacementCompleted || p == ReplacementCancelled || p == ReplacementRejected
}

// ReplacementFailure is a safe, named error that survives coordinator restart.
type ReplacementFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// ReplacementSuccessorController binds the first committed native owner to the
// verifier prepared for that launch. Generic operation saves cannot assign it.
type ReplacementSuccessorController struct {
	Owner              SessionControllerOwner `json:"owner"`
	CapabilityVerifier string                 `json:"capabilityVerifier"`
	CommittedAt        time.Time              `json:"committedAt"`
}

// ReplacementBootstrap separates durable prompt intent, acceptance and settlement.
type ReplacementBootstrap struct {
	PromptHash string     `json:"promptHash"`
	IntentAt   time.Time  `json:"intentAt"`
	TurnID     string     `json:"turnId,omitempty"`
	SettledAt  *time.Time `json:"settledAt,omitempty"`
}

// ReplacementContinuityProbe binds one post-resume round trip to its nonce.
type ReplacementContinuityProbe struct {
	Nonce      string     `json:"nonce"`
	PromptHash string     `json:"promptHash"`
	IntentAt   time.Time  `json:"intentAt"`
	TurnID     string     `json:"turnId,omitempty"`
	SettledAt  *time.Time `json:"settledAt,omitempty"`
}

// ReplacementWriterActivation records a lease move without carrying its token.
type ReplacementWriterActivation struct {
	BeforeEpoch        int64                  `json:"beforeEpoch"`
	AfterEpoch         int64                  `json:"afterEpoch"`
	Owner              SessionControllerOwner `json:"owner"`
	CapabilityVerifier string                 `json:"capabilityVerifier"`
	ActivatedAt        time.Time              `json:"activatedAt"`
}

// ReplacementWorkerNotification retains the delivery boundary for PR 4.
// Pending never means accepted, read, acted, or a worker directive ACK.
type ReplacementWorkerNotification struct {
	SessionID     SessionID              `json:"sessionId"`
	ContactID     string                 `json:"contactId"`
	ObservedOwner SessionControllerOwner `json:"observedOwner"`
	State         string                 `json:"state"`
	CreatedAt     time.Time              `json:"createdAt"`
}

// ReplacementResumeEvidence retains observations from one controlled probe.
type ReplacementResumeEvidence struct {
	BeforeCapabilityVerifier string                 `json:"beforeCapabilityVerifier,omitempty"`
	AfterCapabilityVerifier  string                 `json:"afterCapabilityVerifier,omitempty"`
	BeforeNativeID           string                 `json:"beforeNativeId"`
	AfterNativeID            string                 `json:"afterNativeId"`
	BeforeOwner              SessionControllerOwner `json:"beforeOwner"`
	AfterOwner               SessionControllerOwner `json:"afterOwner"`
	StopConfirmed            bool                   `json:"stopConfirmed"`
	HistoryVerified          bool                   `json:"historyVerified"`
	VerifiedAt               *time.Time             `json:"verifiedAt,omitempty"`

	StartedAt        time.Time                  `json:"startedAt,omitempty"`
	StopReceipt      *ReplacementControllerStop `json:"stopReceipt,omitempty"`
	ResumeIntentAt   *time.Time                 `json:"resumeIntentAt,omitempty"`
	ResumeGeneration string                     `json:"resumeGeneration,omitempty"`
	ResumedAt        *time.Time                 `json:"resumedAt,omitempty"`

	Probe *ReplacementContinuityProbe `json:"probe,omitempty"`
}

// ReplacementPreparation records prompt intent before delivery and retains the
// author receipt independently of the agent's still-running control turn.
type ReplacementPreparation struct {
	IntakeRevision         int64       `json:"intakeRevision"`
	AttemptID              string      `json:"attemptId"`
	Nonce                  string      `json:"nonce"`
	ControlClientID        string      `json:"controlClientId"`
	ControlTurnID          string      `json:"controlTurnId,omitempty"`
	PromptHash             string      `json:"promptHash,omitempty"`
	PromptIntentAt         *time.Time  `json:"promptIntentAt,omitempty"`
	StartedAt              time.Time   `json:"startedAt"`
	DeadlineAt             time.Time   `json:"deadlineAt"`
	LeaseEpoch             int64       `json:"leaseEpoch,omitempty"`
	BaselineHash           string      `json:"baselineHash,omitempty"`
	BaselineWriteReceiptID string      `json:"baselineWriteReceiptId,omitempty"`
	ScratchDir             string      `json:"scratchDir,omitempty"`
	ExpectedSessions       []SessionID `json:"expectedSessions"`
	WriteReceiptID         string      `json:"writeReceiptId,omitempty"`
	SubmittedPath          string      `json:"submittedPath,omitempty"`
	SubmittedHash          string      `json:"submittedHash,omitempty"`
	SubmittedAt            *time.Time  `json:"submittedAt,omitempty"`
	VerifiedAt             *time.Time  `json:"verifiedAt,omitempty"`
}

// ReplacementControllerStop records a positively confirmed operation-owned shutdown.
type ReplacementControllerStop struct {
	Owner              SessionControllerOwner `json:"owner"`
	CapabilityVerifier string                 `json:"capabilityVerifier"`
	NativeID           string                 `json:"nativeId"`
	Method             string                 `json:"method"`
	ConfirmedAt        time.Time              `json:"confirmedAt"`
}

// ReplacementTranscript retains verified cutoffs and immutable publication facts.
type ReplacementTranscript struct {
	FrozenHandoff       []byte                  `json:"frozenHandoff"`
	ExportedAt          *time.Time              `json:"exportedAt,omitempty"`
	CapturedAt          *time.Time              `json:"capturedAt,omitempty"`
	NativeCutoffs       []TranscriptEpochCutoff `json:"nativeCutoffs,omitempty"`
	SnapshotFingerprint string                  `json:"snapshotFingerprint,omitempty"`
	Receipt             *HandoffEvidenceReceipt `json:"receipt,omitempty"`
	TranscriptHash      string                  `json:"transcriptHash,omitempty"`
	AttemptID           string                  `json:"attemptId"`
	AO                  TranscriptSourceCutoff  `json:"ao"`
}

// ReplacementAttempt preserves a retired preparation and cutoff when a safe
// pre-stop retry starts a new attempt. Evidence files remain create-only.
type ReplacementAttempt struct {
	SourceIntakeReleasedAt *time.Time `json:"sourceIntakeReleasedAt,omitempty"`

	ID          string                  `json:"id"`
	Phase       ReplacementPhase        `json:"phase"`
	Preparation *ReplacementPreparation `json:"preparation,omitempty"`
	Transcript  *ReplacementTranscript  `json:"transcript,omitempty"`
	FrozenAt    *time.Time              `json:"frozenAt,omitempty"`
	Error       *ReplacementFailure     `json:"error,omitempty"`
	EndedAt     time.Time               `json:"endedAt"`
}

// ReplacementTarget is the validated launch choice retained by an operation.
type ReplacementTarget struct {
	AgentConfig AgentConfig
	AccountID   string
}

// OrchestratorReplacement contains durable saga facts. HTTP exposes a separate
// read DTO, never a dump of internal configuration or control credentials.
type OrchestratorReplacement struct {
	SchemaVersion            int                        `json:"schemaVersion"`
	ID                       string                     `json:"id"`
	OperationKey             string                     `json:"operationKey"`
	RequestHash              string                     `json:"requestHash"`
	ProjectID                ProjectID                  `json:"projectId"`
	SourceSessionID          SessionID                  `json:"sourceSessionId"`
	SourceOwner              SessionControllerOwner     `json:"sourceOwner"`
	SourceCapabilityVerifier string                     `json:"sourceCapabilityVerifier,omitempty"`
	SourceStopIntentAt       *time.Time                 `json:"sourceStopIntentAt,omitempty"`
	SourceStop               *ReplacementControllerStop `json:"sourceStop,omitempty"`
	SourceRetiredAt          *time.Time                 `json:"sourceRetiredAt,omitempty"`
	ProjectFingerprint       string                     `json:"projectFingerprint,omitempty"`
	TargetAgentConfig        AgentConfig                `json:"targetAgentConfig,omitempty"`
	TargetAccountID          string                     `json:"targetAccountId,omitempty"`
	SharedDir                string                     `json:"sharedDir,omitempty"`
	DocumentPath             string                     `json:"documentPath,omitempty"`
	Preparation              *ReplacementPreparation    `json:"preparation,omitempty"`
	FrozenAt                 *time.Time                 `json:"frozenAt,omitempty"`
	Transcript               *ReplacementTranscript     `json:"transcript,omitempty"`
	SuccessorSessionID       SessionID                  `json:"successorSessionId,omitempty"`
	SpawnRequestID           string                     `json:"spawnRequestId,omitempty"`
	SpawnRequestHash         string                     `json:"spawnRequestHash,omitempty"`
	SpawnRetryGeneration     int64                      `json:"spawnRetryGeneration,omitempty"`
	SpawnAttempts            []ReplacementSpawnAttempt  `json:"spawnAttempts,omitempty"`
	BootstrapClientID        string                     `json:"bootstrapClientId,omitempty"`
	ResumeProbeClientID      string                     `json:"resumeProbeClientId,omitempty"`
	IntakeReleasedAt         *time.Time                 `json:"intakeReleasedAt,omitempty"`
	IntakeDrainRequestedAt   *time.Time                 `json:"intakeDrainRequestedAt,omitempty"`
	SourceIntakeReleasedAt   *time.Time                 `json:"sourceIntakeReleasedAt,omitempty"`
	Phase                    ReplacementPhase           `json:"phase"`
	ResumePhase              ReplacementPhase           `json:"resumePhase,omitempty"`
	Error                    *ReplacementFailure        `json:"error,omitempty"`
	ResumeEvidence           *ReplacementResumeEvidence `json:"resumeEvidence,omitempty"`
	CreatedAt                time.Time                  `json:"createdAt"`
	UpdatedAt                time.Time                  `json:"updatedAt"`
	DeadlineAt               time.Time                  `json:"deadlineAt"`
	Revision                 int64                      `json:"-"`
	IntakeRevision           int64                      `json:"-"`

	SuccessorController *ReplacementSuccessorController `json:"successorController,omitempty"`
	Bootstrap           *ReplacementBootstrap           `json:"bootstrap,omitempty"`

	WriterActivation *ReplacementWriterActivation    `json:"writerActivation,omitempty"`
	Notifications    []ReplacementWorkerNotification `json:"notifications,omitempty"`

	Attempts []ReplacementAttempt `json:"attempts,omitempty"`

	IntakeTransfer *ReplacementIntakeTransfer `json:"intakeTransfer,omitempty"`
}

// CanReleaseSourceIntake authorizes only ordinary input to the unchanged live
// source while preparation is paused. It never authorizes stop, spawn or retry.
func (op OrchestratorReplacement) CanReleaseSourceIntake() bool {
	waiting := op.Phase == ReplacementAwaitingSource || op.Phase == ReplacementAwaitingHandoff || op.Phase == ReplacementAwaitingTranscript
	return waiting && op.SourceStopIntentAt == nil && op.SourceStop == nil && op.SourceRetiredAt == nil && op.SuccessorSessionID == ""
}

// ReplacementProjectFingerprint detects changes to launch rules, hooks, paths
// and settings without copying environment secrets into an operation read model.
func ReplacementProjectFingerprint(p ProjectRecord) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// AllowsChatQuiescence names the controller a replacement may park. A durable
// hibernation marker is only intent; the runtime must still confirm shutdown.
func (op OrchestratorReplacement) AllowsChatQuiescence(rec SessionRecord) bool {
	if rec.IsTerminated || rec.Mode != SessionModeChat || rec.Kind != KindOrchestrator || rec.Harness != HarnessClaudeCode || op.FrozenAt == nil || op.Transcript == nil {
		return false
	}
	switch op.Phase {
	case ReplacementExportingTranscript:
		return op.SourceStopIntentAt != nil && op.SourceRetiredAt == nil && rec.ID == op.SourceSessionID && op.Preparation != nil && op.Preparation.VerifiedAt != nil && rec.ControllerOwner() == op.SourceOwner && op.SourceCapabilityVerifier != "" && rec.Metadata.BrowserCapabilityVerifier == op.SourceCapabilityVerifier
	case ReplacementVerifyingResume:
		proof := op.ResumeEvidence
		return op.SourceRetiredAt != nil && op.Bootstrap != nil && op.Bootstrap.SettledAt != nil && rec.ID == op.SuccessorSessionID && proof != nil && !proof.StartedAt.IsZero() && !proof.StopConfirmed && proof.BeforeNativeID != "" && proof.BeforeNativeID == rec.Metadata.ProviderConversationID && proof.BeforeOwner == rec.ControllerOwner() && proof.BeforeCapabilityVerifier != "" && proof.BeforeCapabilityVerifier == rec.Metadata.BrowserCapabilityVerifier
	}
	return false
}

// AllowsTUIQuiescence authorizes a release intent, not a shutdown receipt.
func (op OrchestratorReplacement) AllowsTUIQuiescence(rec SessionRecord) bool {
	return op.Phase == ReplacementExportingTranscript && op.FrozenAt != nil && op.SourceStopIntentAt != nil && op.SourceRetiredAt == nil && op.Transcript != nil && op.Preparation != nil && op.Preparation.VerifiedAt != nil &&
		rec.ID == op.SourceSessionID && rec.ProjectID == op.ProjectID && !rec.IsTerminated && rec.Kind == KindOrchestrator && rec.Harness == HarnessClaudeCode && NormalizeSessionMode(rec.Mode) == SessionModeTUI && rec.Activity.State == ActivityIdle && !rec.Metadata.ConversationCheckpointUnsettled &&
		rec.ControllerOwner() == op.SourceOwner && op.SourceCapabilityVerifier != "" && rec.Metadata.BrowserCapabilityVerifier == op.SourceCapabilityVerifier &&
		rec.Metadata.RuntimeHandleID != "" && rec.Metadata.RuntimeLaunchID != "" && rec.Metadata.AgentSessionID != "" && rec.Metadata.AgentSessionIDLaunchID == rec.Metadata.RuntimeLaunchID
}

// MatchesSource checks shutdown evidence against current durable controller
// facts. Runtime confirmation must already have established the actual stop.
func (stop ReplacementControllerStop) MatchesSource(rec SessionRecord) bool {
	if rec.IsTerminated || stop.ConfirmedAt.IsZero() || stop.Owner != rec.ControllerOwner() || stop.CapabilityVerifier == "" || stop.CapabilityVerifier != rec.Metadata.BrowserCapabilityVerifier || stop.NativeID == "" {
		return false
	}
	switch NormalizeSessionMode(rec.Mode) {
	case SessionModeChat:
		return stop.Method == "chat-hibernate" && rec.HibernatedAt != nil && stop.NativeID == rec.Metadata.ProviderConversationID
	case SessionModeTUI:
		return stop.Method == "tui-fenced-stop" && rec.Activity.State == ActivityExited && rec.Metadata.RuntimeReleasedLaunchID != "" && rec.Metadata.RuntimeReleasedLaunchID == rec.Metadata.RuntimeLaunchID && stop.NativeID == rec.Metadata.AgentSessionID
	}
	return false
}
