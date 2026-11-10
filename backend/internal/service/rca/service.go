package rca

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/components/hcg/policy"
	rcapolicy "github.com/aoagents/agent-orchestrator/components/hcg/rca"
)

// MetadataStore reuses existing session, binding and source read ports.
type MetadataStore interface {
	scopeStore
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
}

// EventsReader reads canonical usage without collecting it.
type EventsReader interface {
	Events(context.Context, domain.SessionID, int64, *int64, int64) (domain.UsageEventsPage, error)
}

// ProjectionStore owns only RCA policy metadata and numeric projections.
type ProjectionStore interface {
	rcapolicy.ManifestStore
	LoadSnapshot(context.Context, domain.ProjectID, domain.SessionID) (domain.RCARecordedState, bool, error)
	StageSnapshot(context.Context, domain.RCARecordedState) error
	FinishSnapshot(context.Context, bool) error
	Ready(context.Context) error
	Resume(context.Context) error
	Reason() string
	Close()
}

type jobState struct{ revision, status, reason string }

// Service coordinates authorized recorded-content estimates.
type Service struct {
	store      MetadataStore
	events     EventsReader
	projection ProjectionStore
	core       *rcapolicy.Consumer
	scope      *scopeResolver
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.Mutex
	jobs       map[domain.SessionID]jobState
	closed     bool
	failed     bool
	queryBusy  atomic.Bool
}

// New never launches a native agent or opens a transcript. Source descriptors
// are resolved only after an explicit, enabled analysis of a selected worker.
func New(ctx context.Context, root string, store MetadataStore, events EventsReader, roots usagesvc.SourceRoots) *Service {
	projection, err := NewDiskStore(root)
	var disk ProjectionStore
	if err == nil {
		disk = projection
	}
	home, _ := os.UserHomeDir()
	allow := []string{
		filepath.Join(home, ".ao", "data", "agent-accounts", "history", "codex", "sessions"),
		filepath.Join(home, ".ao", "data", "agent-accounts", "history", "codex", "archived_sessions"),
		filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".codex", "archived_sessions"),
	}
	return newService(ctx, store, events, disk, &scopeResolver{store: store, roots: []string{roots.AccountCodexSessions, roots.AccountCodexArchived, roots.CodexSessions, roots.CodexArchived}, allowlist: allow})
}

func newService(ctx context.Context, store MetadataStore, events EventsReader, disk ProjectionStore, scope *scopeResolver) *Service {
	ctx, cancel := context.WithCancel(ctx)
	var manifests rcapolicy.ManifestStore
	if disk != nil {
		manifests = disk
	}
	return &Service{store: store, events: events, projection: disk, core: rcapolicy.New(ctx, manifests), scope: scope, ctx: ctx, cancel: cancel, jobs: map[domain.SessionID]jobState{}}
}

// Close cancels RCA jobs and releases owned handles.
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.core.Close()
	s.wg.Wait()
	if s.projection != nil {
		s.projection.Close()
	}
}

// ProjectPolicyChanged revokes jobs when a durable RCA parent changes.
func (s *Service) ProjectPolicyChanged(id domain.ProjectID, in domain.RCAPolicySnapshot) {
	if id == "agent-orchestrator" {
		s.core.ProjectPolicyChanged(string(id), "worker", policy.Inputs{Project: in.Project, Role: in.Worker})
		s.core.ProjectPolicyChanged(string(id), "orchestrator", policy.Inputs{Project: in.Project, Role: in.Orchestrator})
	}
}
func sessionKey(id domain.SessionID) rcapolicy.Key {
	return rcapolicy.Key{ProjectID: "agent-orchestrator", SessionID: string(id)}
}

func (s *Service) facts(ctx context.Context, id domain.SessionID) (domain.SessionRecord, policy.Inputs, error) {
	rec, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return rec, policy.Inputs{}, apierr.Internal("RCA_UNAVAILABLE", "source_metadata_failed")
	}
	if !ok {
		return rec, policy.Inputs{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if rec.ProjectID != "agent-orchestrator" || rec.Kind != domain.KindWorker || rec.Harness != domain.HarnessCodex {
		return rec, policy.Inputs{}, apierr.Forbidden("RCA_SCOPE_NOT_ALLOWED", "scope_not_allowed")
	}
	project, ok, err := s.store.GetProject(ctx, string(rec.ProjectID))
	if err != nil || !ok {
		return rec, policy.Inputs{}, apierr.Internal("RCA_UNAVAILABLE", "source_metadata_failed")
	}
	in := project.Config.RCAPolicySnapshot()
	return rec, policy.Inputs{Project: in.Project, Role: in.Worker}, nil
}

func policyView(v rcapolicy.View) domain.RCAPolicyView {
	return domain.RCAPolicyView{Project: v.Inputs.Project, Role: v.Inputs.Role, Session: v.Inputs.Session, Source: v.Source, SessionOverride: v.SessionOverride, EffectiveEnabled: v.EffectiveEnabled, EmergencyStop: v.EmergencyStop, ManifestRevision: v.ManifestRevision}
}

// Get reads a numeric view; Analyze explicitly requests a new snapshot.
func (s *Service) Get(ctx context.Context, id domain.SessionID, q domain.RCAQuery) (domain.RCAComposition, error) {
	key := sessionKey(id)
	receipt := s.core.Revision(key)
	rec, inputs, err := s.facts(ctx, id)
	if err != nil {
		return domain.RCAComposition{}, err
	}
	out := domain.RCAComposition{ProjectID: rec.ProjectID, SessionID: id, Status: "unavailable", Reason: "analysis_not_requested", RequestComposition: "UNKNOWN", Baseline: baseline(approvedBaseline)}
	page, err := s.events.Events(ctx, id, q.AfterID, q.ThroughID, q.Limit)
	if err != nil {
		return out, err
	}
	out.Canonical = canonicalProjection(page)
	busy := func(reason string) domain.RCAComposition {
		view, _ := s.core.CachedView(ctx, key, inputs)
		out.Policy = policyView(view)
		out.Status, out.Reason = "unavailable", reason
		s.mu.Lock()
		job := s.jobs[id]
		failed := s.failed
		s.mu.Unlock()
		if failed {
			out.Reason = "analysis_failed"
		} else if view.EffectiveEnabled && job.status == "running" && reason == "writer_busy" {
			out.Status = "running"
		}
		return out
	}
	if !s.queryBusy.CompareAndSwap(false, true) {
		return busy("writer_busy"), nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.queryBusy.Store(false)
		return busy("canceled"), nil
	}
	s.wg.Add(1)
	s.mu.Unlock()
	type numericResult struct {
		view           rcapolicy.View
		recorded       *domain.RCARecordedState
		status, reason string
		blocked        bool
	}
	result := make(chan numericResult, 1)
	readCtx, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(ctx, cancel)
	go func() {
		value := numericResult{status: "unavailable", reason: "analysis_not_requested"}
		defer func() {
			if recover() != nil {
				s.mu.Lock()
				s.failed = true
				s.mu.Unlock()
				s.core.Unavailable()
				value = numericResult{status: "unavailable", reason: "analysis_failed", blocked: true}
			}
			stop()
			cancel()
			s.queryBusy.Store(false)
			s.wg.Done()
			result <- value
		}()
		view, why := s.core.View(readCtx, key, inputs)
		value.view = view
		s.mu.Lock()
		job, hasJob := s.jobs[id]
		failed := s.failed
		s.mu.Unlock()
		if failed {
			value.reason = "analysis_failed"
			value.blocked = true
			return
		}
		if why != "" {
			value.reason = why
			value.blocked = true
			if why == "storage_unavailable" {
				value.reason = s.storageReason()
			}
			if hasJob && job.status == "unavailable" {
				value.reason = job.reason
			}
			if why == "writer_busy" && hasJob && job.status == "running" && view.EffectiveEnabled {
				value.status = "running"
			}
			return
		}
		if !view.EffectiveEnabled {
			value.reason = "disabled"
			value.blocked = true
			return
		}
		if hasJob && job.status == "running" {
			value.status, value.reason = "running", "analysis_running"
			value.blocked = true
			return
		}
		if s.projection == nil {
			value.reason = "storage_unavailable"
			value.blocked = true
			return
		}
		loadFailed := false
		why = s.core.WithStorage(readCtx, func(ctx context.Context) error {
			load := func() error {
				row, found, err := s.projection.LoadSnapshot(ctx, rec.ProjectID, id)
				if err != nil {
					loadFailed = true
					return err
				}
				if found {
					value.recorded = &row
					value.status, value.reason = row.Status, row.Reason
				}
				return nil
			}
			if err := load(); err != nil {
				return err
			}
			if err := s.projection.Ready(ctx); err != nil {
				return err
			}
			if hasJob && job.status != "running" && (value.recorded == nil || value.recorded.ScopeRevision != job.revision) {
				return load()
			}
			return nil
		})
		if why != "" {
			value.recorded = nil
			value.status, value.reason = "unavailable", why
			value.blocked = true
			if loadFailed {
				value.reason = "storage_read_failed"
				s.core.Unavailable()
			} else if why == "storage_unavailable" {
				value.reason = s.storageReason()
				s.core.Unavailable()
			}
			if why == "writer_busy" && hasJob && job.status == "running" {
				value.status = "running"
			}
		}
	}()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	var value numericResult
	select {
	case value = <-result:
	case <-ctx.Done():
		cancel()
		return busy("canceled"), nil
	case <-timer.C:
		cancel()
		return busy("writer_busy"), nil
	}
	out.Policy = policyView(value.view)
	out.Recorded = value.recorded
	out.Status, out.Reason = value.status, value.reason
	if value.blocked {
		fresh, _ := s.core.CachedView(ctx, key, inputs)
		out.Policy = policyView(fresh)
		if !fresh.EffectiveEnabled && out.Status == "running" {
			out.Status = "unavailable"
		}
		return out, nil
	}
	if q.Analyze {
		ticket, _, why := s.core.Begin(s.ctx, key, inputs, receipt)
		if why == "" {
			if why := s.core.Reserve(ticket); why != "" {
				s.core.Finish(ticket)
				out.Recorded = nil
				out.Status, out.Reason = "unavailable", why
				return out, nil
			}
			job := jobState{revision: uuid.NewString(), status: "running", reason: "analysis_running"}
			s.mu.Lock()
			if s.closed || s.failed {
				s.mu.Unlock()
				s.core.Finish(ticket)
				out.Status, out.Reason = "unavailable", "canceled"
				out.Recorded = nil
				return out, nil
			}
			s.jobs[id] = job
			s.wg.Add(1)
			s.mu.Unlock()
			go s.analyze(ticket, rec, job.revision)
		} else if why != "analysis_running" {
			out.Status, out.Reason = "unavailable", why
			out.Recorded = nil
			return out, nil
		}
	}
	s.mu.Lock()
	job, hasJob := s.jobs[id]
	failed, closed := s.failed, s.closed
	s.mu.Unlock()
	if hasJob {
		out.Status, out.Reason = job.status, job.reason
		if job.status != "running" && out.Recorded == nil && job.status != "unavailable" {
			out.Status, out.Reason = "unavailable", "analysis_not_requested"
		}
	}
	if failed || closed || ctx.Err() != nil || !s.core.Current(receipt) {
		out.Recorded = nil
		out.Status, out.Reason = "unavailable", "policy_changed"
		if failed {
			out.Reason = "analysis_failed"
		}
	}
	return out, nil
}

func canonicalProjection(p domain.UsageEventsPage) domain.RCACanonicalPage {
	out := domain.RCACanonicalPage{ThroughID: p.ThroughID, NextAfterID: p.NextAfterID, HasMore: p.HasMore, MetadataAsOf: p.MetadataAsOf, SourceAsOf: p.SourceAsOf, Events: []domain.RCACanonicalEvent{}}
	for _, e := range p.Events {
		out.Events = append(out.Events, domain.RCACanonicalEvent{ID: e.ID, BindingID: e.BindingID, UsageSourceID: e.UsageSourceID, InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CachedInputTokens: e.CachedInputTokens})
	}
	return out
}

func (s *Service) storageReason() string {
	if s.projection == nil || s.projection.Reason() == "" {
		return "storage_unavailable"
	}
	return s.projection.Reason()
}

func (s *Service) analyze(ticket *rcapolicy.Ticket, rec domain.SessionRecord, revision string) {
	defer s.wg.Done()
	defer s.core.Finish(ticket)
	defer func() {
		if recover() != nil {
			// Source content can occur in a panic value. Do not log, format or
			// capture it; revoke RCA jobs and require explicit recovery.
			s.mu.Lock()
			s.failed = true
			if current, ok := s.jobs[rec.ID]; ok && current.revision == revision {
				s.jobs[rec.ID] = jobState{revision: revision, status: "unavailable", reason: "analysis_failed"}
			}
			s.mu.Unlock()
			s.core.Unavailable()
		}
	}()
	value := domain.RCARecordedState{ProjectID: rec.ProjectID, SessionID: rec.ID, ScopeRevision: revision, ObservedAt: time.Now().UTC(), Status: "ready", Snapshots: []domain.RCARecordedSnapshot{}}
	sources, reason := s.scope.resolve(ticket.Context(), rec.ID)
	if reason == "" {
		for _, source := range sources {
			if source.owner != stamp(rec) {
				reason = "policy_changed"
				break
			}
			file, why := source.open(ticket.Context())
			if why != "" {
				reason = why
				break
			}
			r := func() recording {
				defer func() { _ = file.Close() }()
				return readCodex(ticket.Context(), file, source.eof, source.nativeID, defaultReadLimits)
			}()
			r.ScopeID, r.Association = uuid.NewString(), source.association
			if source.sourceID > 0 {
				binding, sourceID, generation := source.bindingID, source.sourceID, source.generation
				r.BindingID, r.UsageSourceID, r.SourceGeneration = &binding, &sourceID, &generation
			}
			value.Snapshots = append(value.Snapshots, domain.RCARecordedSnapshot(r))
			if r.Status != "ready" {
				value.Status = "partial"
			}
		}
	}
	if reason == "" {
		for _, source := range sources {
			if !s.scope.current(ticket.Context(), source) {
				reason = "policy_changed"
				break
			}
		}
	}
	if reason == "" {
		_, current, err := s.facts(ticket.Context(), rec.ID)
		if err != nil {
			reason = "policy_changed"
		} else if _, why := s.core.View(ticket.Context(), sessionKey(rec.ID), current); why != "" {
			reason = why
		}
	}
	if reason == "" {
		observed := false
		for _, snapshot := range value.Snapshots {
			for _, bucket := range snapshot.Buckets {
				if bucket.Bytes != nil && *bucket.Bytes > 0 {
					observed = true
				}
			}
		}
		if !observed {
			value.Status, value.Reason = "unavailable", "no_observed_content"
		}
		reason = s.core.Commit(ticket, func(ctx context.Context) error { return s.projection.StageSnapshot(ctx, value) }, nil, s.projection.FinishSnapshot)
		if reason == "storage_unavailable" {
			reason = s.storageReason()
			s.core.Unavailable()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.jobs[rec.ID]; ok && current.revision == revision {
		if reason != "" {
			current.status, current.reason = "unavailable", reason
		} else {
			current.status, current.reason = value.Status, value.Reason
		}
		s.jobs[rec.ID] = current
	}
}

func (s *Service) resume(ctx context.Context) error {
	if s.projection == nil {
		return apierr.Internal("RCA_UNAVAILABLE", "storage_unavailable")
	}
	if why := s.core.Recover(ctx, s.projection.Resume); why != "" {
		if why == "revocation_persistence_pending" || why == "writer_busy" {
			return apierr.Internal("RCA_UNAVAILABLE", why)
		}
		return apierr.Internal("RCA_UNAVAILABLE", s.storageReason())
	}
	s.mu.Lock()
	s.failed = false
	s.mu.Unlock()
	return nil
}

// SetOverride revokes work and changes only the session RCA override.
func (s *Service) SetOverride(ctx context.Context, id domain.SessionID, enabled *bool) (domain.RCAPolicyView, error) {
	_, inputs, err := s.facts(ctx, id)
	if err != nil {
		return domain.RCAPolicyView{}, err
	}
	// Revoke existing work even when storage rejects the control write.
	s.core.Invalidate(sessionKey(id))
	if enabled == nil || *enabled {
		if err := s.resume(ctx); err != nil {
			return domain.RCAPolicyView{}, err
		}
	}
	v, why := s.core.SetOverride(ctx, sessionKey(id), inputs, enabled)
	if why != "" {
		return domain.RCAPolicyView{}, apierr.Internal("RCA_UNAVAILABLE", why)
	}
	return policyView(v), nil
}

// EmergencyStop controls host RCA work independently of native agents.
func (s *Service) EmergencyStop(ctx context.Context, stop bool) error {
	if !stop {
		if err := s.resume(ctx); err != nil {
			return err
		}
	}
	if why := s.core.EmergencyStop(ctx, stop); why != "" {
		return apierr.Internal("RCA_UNAVAILABLE", why)
	}
	return nil
}

// Purge removes RCA numbers and scope without deleting native/canonical data.
func (s *Service) Purge(ctx context.Context, id domain.SessionID) error {
	if _, _, err := s.facts(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.jobs, id)
	s.mu.Unlock()
	if why := s.core.Purge(ctx, sessionKey(id)); why != "" {
		return apierr.Internal("RCA_UNAVAILABLE", why)
	}
	return nil
}
