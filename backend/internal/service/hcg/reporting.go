// Package hcg connects AO-owned session facts to the optional fixed report
// consumer. Canonical usage is read only here; external reports never repair it.
package hcg

import (
	"context"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

// Store exposes only AO identity, policy and source reads to reporting.
type Store interface {
	ListProjects(context.Context) ([]domain.ProjectRecord, error)
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	ListUsageBindingsForSession(context.Context, domain.SessionID) ([]domain.UsageBindingRecord, error)
	ListUsageSourcesForBinding(context.Context, int64) ([]domain.UsageSourceRecord, error)
}

// Service keeps optional reports separate from native execution and canonical writes.
type Service struct {
	delegation *reporting.DelegationConsumer
	store      Store
	root       string
	core       *reporting.Consumer
	metadata   *metadataObserver
}

// New probes an existing registration at startup; failure disables only reporting.
func New(ctx context.Context, root string, store Store) *Service {
	return NewWithDelegationProvider(ctx, root, store, nil, nil)
}

// NewWithDelegationProvider supports the fixed offline provider fixture. The
// production daemon passes no provider/credentials and cannot make paid calls.
func NewWithDelegationProvider(ctx context.Context, root string, store Store, provider reporting.DelegationProvider, checkpoint func(context.Context, string)) *Service {
	c, err := reporting.New(root)
	s := &Service{store: store, root: root, core: c}
	if err == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c.Probe(probeCtx)
	}
	if writer, ok := store.(interface {
		InsertExecutionUsage(context.Context, domain.ExecutionUsageReceipt, string) error
	}); ok {
		s.delegation = reporting.NewDelegation(root, delegationReceiptRecorder{canonical: usagesvc.NewExecutionRecorder(writer)}, provider, checkpoint)
		recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 5*time.Second)
		// Failed projections retain their pending receipt and worst hold. Startup
		// never retries the provider or changes native session/policy state.
		if grant := s.delegation.LabGrant(); grant == nil || s.validateLabOwner(recoveryCtx, grant) == nil {
			_ = s.delegation.RecoverDelegationReceipts(recoveryCtx)
		}
		recoveryCancel()
	}
	s.startMetadata(ctx)
	return s
}

func (s *Service) identity(rec domain.SessionRecord) reporting.Identity {
	idn := reporting.Identity{DataRoot: s.root, ProjectID: string(rec.ProjectID), SessionID: string(rec.ID), Role: string(rec.Kind), Mode: string(domain.NormalizeSessionMode(rec.Mode)), Harness: string(rec.Harness), Worktree: rec.Metadata.WorkspacePath, ArtifactDir: rec.Metadata.ArtifactDir, ControllerGeneration: rec.Metadata.ControllerGeneration}
	if rec.Mode == domain.SessionModeChat {
		idn.LaunchID, idn.NativeID = rec.Metadata.AgentSettingsLaunchID, rec.Metadata.ProviderConversationID
	} else {
		idn.LaunchID = rec.Metadata.RuntimeLaunchID
		if rec.Metadata.AgentSessionIDLaunchID == idn.LaunchID && idn.LaunchID != "" {
			idn.NativeID = rec.Metadata.AgentSessionID
		}
	}
	return idn
}

func (s *Service) facts(ctx context.Context, id domain.SessionID) (reporting.Identity, reporting.Inputs, error) {
	rec, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return reporting.Identity{}, reporting.Inputs{}, err
	}
	if !ok {
		return reporting.Identity{}, reporting.Inputs{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	return s.recordFacts(ctx, rec)
}

func (s *Service) recordFacts(ctx context.Context, rec domain.SessionRecord) (reporting.Identity, reporting.Inputs, error) {
	idn := s.identity(rec)
	inputs := reporting.Inputs{}
	if rec.ProjectID != "" {
		p, ok, err := s.store.GetProject(ctx, string(rec.ProjectID))
		if err != nil {
			return idn, inputs, err
		}
		if !ok {
			return idn, inputs, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
		}
		inputs.Project = p.Config.HCGReporting.Enabled
		switch rec.Kind {
		case domain.KindWorker:
			inputs.Role = p.Config.Worker.HCGReporting.Enabled
		case domain.KindOrchestrator:
			inputs.Role = p.Config.Orchestrator.HCGReporting.Enabled
		}
	}
	return idn, inputs, nil
}

func unavailable() error {
	return apierr.Unavailable("HCG_REPORTING_UNAVAILABLE", "Fixed reporting integration is unavailable")
}

// Get reads effective policy without creating a missing manifest.
func (s *Service) Get(ctx context.Context, id domain.SessionID) (reporting.View, error) {
	i, in, err := s.facts(ctx, id)
	if err != nil {
		return reporting.View{}, err
	}
	if s.core == nil {
		return reporting.View{}, unavailable()
	}
	return s.core.Get(ctx, i, in), nil
}

// SetOverride persists an explicit value, or clears it to inheritance when nil.
func (s *Service) SetOverride(ctx context.Context, id domain.SessionID, enabled *bool) (reporting.View, error) {
	i, in, err := s.facts(ctx, id)
	if err != nil {
		return reporting.View{}, err
	}
	if s.core == nil || s.core.SetOverride(ctx, i, in, enabled) != nil {
		return reporting.View{}, unavailable()
	}
	return s.core.Get(ctx, i, in), nil
}

// EmergencyStop controls only the host-wide reporting consumer.
func (s *Service) EmergencyStop(ctx context.Context, enabled bool) error {
	if s.core == nil || s.core.EmergencyStop(ctx, enabled) != nil {
		return unavailable()
	}
	return nil
}

// Register checks a deployment-supplied fixed engine, without installing it.
func (s *Service) Register(ctx context.Context, executable, version string) (reporting.EngineStatus, error) {
	if s.core == nil {
		return reporting.EngineStatus{}, unavailable()
	}
	status, err := s.core.Register(ctx, executable, version)
	if err != nil {
		return status, unavailable()
	}
	return status, nil
}

// Report returns a disposable external snapshot of current authorized native files.
func (s *Service) Report(ctx context.Context, id domain.SessionID) (reporting.Snapshot, error) {
	rec, exists, err := s.store.GetSession(ctx, id)
	if err != nil {
		return reporting.Snapshot{}, err
	}
	if !exists {
		return reporting.Snapshot{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if s.core == nil {
		return reporting.Snapshot{}, unavailable()
	}
	// Capture before reading any policy. An off/on ABA invalidates this request
	// even if it has not reached the consumer's jobs set yet.
	revision := s.core.PolicyRevision(s.identity(rec))
	i, in, err := s.recordFacts(ctx, rec)
	if err != nil {
		return reporting.Snapshot{}, err
	}
	// Disabled requests do not inspect native files or launch a tool.
	view := s.core.Get(ctx, i, in)
	files := []reporting.NativeFile{}
	if view.Active {
		bindings, err := s.store.ListUsageBindingsForSession(ctx, id)
		if err != nil {
			return reporting.Snapshot{}, err
		}
		for _, b := range bindings {
			if b.SessionID != id || string(b.Harness) != i.Harness || b.NativeRootID != i.NativeID {
				continue
			}
			sources, err := s.store.ListUsageSourcesForBinding(ctx, b.ID)
			if err != nil {
				return reporting.Snapshot{}, err
			}
			for _, src := range sources {
				source := ""
				switch src.Kind {
				case domain.UsageSourceClaudeMain:
					source = "claude"
				case domain.UsageSourceCodexRollout:
					source = "codex"
				}
				if source != "" && src.NativeSessionID == i.NativeID && src.SubagentID == "" && filepath.IsAbs(src.ArtifactPath) {
					files = append(files, reporting.NativeFile{Root: filepath.Dir(src.ArtifactPath), Path: filepath.Base(src.ArtifactPath), Source: source, NativeID: i.NativeID})
				}
			}
		}
	}
	out := s.core.Report(ctx, i, in, files, revision)
	if out.Status == "ok" {
		current, currentInputs, err := s.facts(ctx, id)
		if err != nil || current.ControllerGeneration != i.ControllerGeneration || current.NativeID != i.NativeID || current.LaunchID != i.LaunchID || current.Harness != i.Harness || current.Mode != i.Mode || !s.core.Get(ctx, current, currentInputs).Active {
			out.Status, out.Reason, out.Events, out.Tools = "unavailable", "policy_or_identity_changed", []reporting.Event{}, []reporting.ToolCounts{}
		}
	}
	return out, nil
}
