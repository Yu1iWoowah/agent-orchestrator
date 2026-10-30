// Package orchestratorreplacement coordinates a durable, project-scoped handoff.
package orchestratorreplacement

import (
	"context"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/projectdocuments"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

// Store retains operation facts independently of the request or coordinator.
type Store interface {
	RecordReplacementResumeUnlaunched(context.Context, string, int64, string, string, time.Time) (domain.OrchestratorReplacement, bool, error)
	SaveReplacementRuntimeProgress(context.Context, domain.OrchestratorReplacement) (bool, error)
	RetryRetainedReplacement(context.Context, string, int64, time.Time, time.Time) (domain.OrchestratorReplacement, bool, error)
	ClearReplacementStopIntent(context.Context, string, int64, ports.ReplacementSourceObservation, time.Time) (domain.OrchestratorReplacement, bool, error)
	ReadReplacementIntake(context.Context, string) (domain.ReplacementIntake, error)
	ReleaseReplacementSourceIntake(context.Context, string, int64, time.Time) (domain.OrchestratorReplacement, bool, error)
	RecordReplacementIntakeDrain(context.Context, string, int64, time.Time) (domain.OrchestratorReplacement, bool, error)
	ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error)
	ResetReplacementBeforeStop(context.Context, string, int64, bool, time.Time, time.Time) (domain.OrchestratorReplacement, bool, error)
	ReleaseReplacementIntake(context.Context, string, int64, time.Time) (domain.OrchestratorReplacement, bool, error)
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	LoadTranscriptSourceSnapshot(context.Context, domain.SessionID) (domain.TranscriptSourceSnapshot, error)
	GetOrchestratorReplacementByKey(context.Context, string) (domain.OrchestratorReplacement, bool, error)
	CreateOrchestratorReplacement(context.Context, domain.OrchestratorReplacement) (domain.OrchestratorReplacement, bool, error)
	GetOrchestratorReplacement(context.Context, string) (domain.OrchestratorReplacement, bool, error)
	GetActiveOrchestratorReplacement(context.Context, domain.ProjectID) (domain.OrchestratorReplacement, bool, error)
	ListActiveOrchestratorReplacements(context.Context) ([]domain.OrchestratorReplacement, error)
	SaveOrchestratorReplacement(context.Context, domain.OrchestratorReplacement) (bool, error)
}

// Runtime preflight resolves the normal launch configuration without starting a
// controller. Provider actions are added at their verified phase boundaries.
type Runtime interface {
	DrainReplacementIntake(context.Context, domain.OrchestratorReplacement) error
	EnsureReplacementProbe(context.Context, domain.OrchestratorReplacement, string, bool) (PreparationDelivery, error)
	ResumeReplacementSuccessor(context.Context, domain.OrchestratorReplacement, bool) (ports.ReplacementResumeObservation, error)
	QuiesceReplacementSuccessor(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error)
	EnsureReplacementBootstrap(context.Context, domain.OrchestratorReplacement, string, bool) (PreparationDelivery, error)
	ObserveReplacementSuccessor(context.Context, domain.OrchestratorReplacement) (SourceObservation, error)
	EnsureReplacementSuccessor(context.Context, domain.OrchestratorReplacement) (domain.SessionRecord, error)
	RetireReplacementSource(context.Context, domain.OrchestratorReplacement, ports.ReplacementRetirementProof) (domain.OrchestratorReplacement, error)
	QuiesceReplacementSource(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error)
	PreflightReplacementTarget(context.Context, domain.ProjectRecord) (domain.ReplacementTarget, error)
	ObserveReplacementSource(context.Context, domain.OrchestratorReplacement) (SourceObservation, error)
	EnsureReplacementPreparation(context.Context, domain.OrchestratorReplacement, string, bool) (PreparationDelivery, error)
}

// SourceObservation identifies the controller and concrete control-turn facts.
// Empty/unknown state or provenance never establishes quiescence.
type SourceObservation = ports.ReplacementSourceObservation

// PreparationDelivery is a provider/queue receipt, distinct from completion.
type PreparationDelivery = ports.ReplacementPreparationDelivery

// Documents locates the AO-owned shared root without writing living files.
type Documents interface {
	ActivateReplacementWriter(context.Context, string, int64) (domain.OrchestratorReplacement, error)
	ReadReplacementHandoff(context.Context, string, int64) (projectdocuments.File, error)
	PrepareReplacementRetirement(context.Context, string, int64, transcriptexport.Output) (projectdocuments.ReplacementRetirementProof, error)
	PublishReplacementEvidence(context.Context, string, int64, transcriptexport.Output) (domain.OrchestratorReplacement, error)
	Info(context.Context, domain.ProjectID) (projectdocuments.Info, error)
	RecordReplacementPreparation(context.Context, string, int64, projectdocuments.ReplacementPreparationInput, func(projectdocuments.File) error) (domain.OrchestratorReplacement, error)
	BeginReplacementPreparation(context.Context, string, int64) (domain.OrchestratorReplacement, error)
	FreezeReplacementHandoff(context.Context, string, int64) (domain.OrchestratorReplacement, error)
}

// Deps binds daemon services; WithProjectGate shares admission with legacy clean.
type Deps struct {
	NativeTranscripts ports.NativeTranscriptReader
	Store             Store
	Runtime           Runtime
	Documents         Documents
	Now               func() time.Time
	WithProjectGate   func(context.Context, domain.ProjectID, func(context.Context) error) error
}

// Service accepts and reconciles operations; it never owns provider processes.
type Service struct {
	deps    Deps
	locksMu sync.Mutex
	locks   map[domain.ProjectID]*sync.Mutex
}

// Request intentionally contains no caller-supplied harness or config override.
type Request struct {
	ProjectID       domain.ProjectID
	SourceSessionID domain.SessionID
	OperationKey    string
}

// New constructs the daemon-owned coordinator.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{deps: d}
}

// Accept durably records one intent before returning to a short-lived caller.
func (s *Service) Accept(ctx context.Context, request Request) (domain.OrchestratorReplacement, error) {
	var op domain.OrchestratorReplacement
	var err error
	accept := func(locked context.Context) error { op, err = s.accept(locked, request); return err }
	if s.deps.WithProjectGate != nil {
		err = s.deps.WithProjectGate(ctx, request.ProjectID, accept)
	} else {
		err = accept(ctx)
	}
	return op, err
}

// Get reads facts even after the request or coordinator that wrote them exits.
func (s *Service) Get(ctx context.Context, id string) (domain.OrchestratorReplacement, error) {
	op, found, err := s.deps.Store.GetOrchestratorReplacement(ctx, id)
	if err == nil && !found {
		return op, operationNotFound()
	}
	return op, err
}
