package rca

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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
	SaveSnapshot(context.Context, domain.RCARecordedState) error
	Purge(context.Context, domain.ProjectID, domain.SessionID) error
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
		s.core.ProjectPolicyChanged(string(id), policy.Inputs{Project: in.Project, Role: in.Worker})
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
	receipt := s.core.Revision(sessionKey(id))
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
	v, why := s.core.View(ctx, sessionKey(id), inputs)
	out.Policy = policyView(v)
	if s.projection != nil {
		cached, ok, _ := s.projection.LoadSnapshot(ctx, rec.ProjectID, id)
		if ok {
			out.Recorded = &cached
			out.Status, out.Reason = cached.Status, cached.Reason
		}
	}
	if why != "" {
		out.Status, out.Reason = "unavailable", why
		return out, nil
	}
	if !v.EffectiveEnabled {
		out.Status, out.Reason = "unavailable", "disabled"
		return out, nil
	}
	s.mu.Lock()
	failed := s.failed
	s.mu.Unlock()
	if failed {
		out.Status, out.Reason = "unavailable", "analysis_failed"
		return out, nil
	}
	ready := s.projection != nil && s.projection.Ready(ctx) == nil
	if !ready {
		out.Status, out.Reason = "unavailable", s.storageReason()
		return out, nil
	}
	if q.Analyze {
		ticket, _, why := s.core.Begin(s.ctx, sessionKey(id), inputs, receipt)
		if why == "" {
			job := jobState{revision: uuid.NewString(), status: "running", reason: "analysis_running"}
			s.mu.Lock()
			if s.closed || s.failed {
				s.mu.Unlock()
				s.core.Finish(ticket)
				out.Status, out.Reason = "unavailable", "canceled"
				return out, nil
			}
			s.jobs[id] = job
			s.wg.Add(1)
			s.mu.Unlock()
			go s.analyze(ticket, rec, job.revision)
		} else if why != "analysis_running" {
			out.Status, out.Reason = "unavailable", why
			return out, nil
		}
	}
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	if ok {
		out.Status, out.Reason = job.status, job.reason
		if job.status != "running" && (out.Recorded == nil || out.Recorded.ScopeRevision != job.revision) {
			if cached, found, _ := s.projection.LoadSnapshot(ctx, rec.ProjectID, id); found {
				out.Recorded = &cached
			} else if job.status != "unavailable" {
				out.Status, out.Reason = "unavailable", "analysis_not_requested"
			}
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
			s.core.Close()
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
		reason = s.core.Commit(ticket, func(ctx context.Context) error { return s.projection.SaveSnapshot(ctx, value) })
		if reason == "storage_unavailable" {
			reason = s.storageReason()
			s.core.Close()
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
	if s.projection == nil || s.projection.Resume(ctx) != nil || s.core.Recover(ctx) != "" {
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
	if err := s.resume(ctx); err != nil {
		return domain.RCAPolicyView{}, err
	}
	v, why := s.core.SetOverride(ctx, sessionKey(id), inputs, enabled)
	if why != "" {
		return domain.RCAPolicyView{}, apierr.Internal("RCA_UNAVAILABLE", s.storageReason())
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
		return apierr.Internal("RCA_UNAVAILABLE", s.storageReason())
	}
	return nil
}

// Purge removes RCA numbers and scope without deleting native/canonical data.
func (s *Service) Purge(ctx context.Context, id domain.SessionID) error {
	rec, _, err := s.facts(ctx, id)
	if err != nil {
		return err
	}
	s.core.Invalidate(sessionKey(id))
	s.mu.Lock()
	delete(s.jobs, id)
	s.mu.Unlock()
	if err := s.resume(ctx); err != nil {
		return err
	}
	if s.projection.Purge(ctx, rec.ProjectID, id) != nil {
		return apierr.Internal("RCA_UNAVAILABLE", s.storageReason())
	}
	return nil
}
