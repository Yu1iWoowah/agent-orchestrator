package delegate

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

type ProfileRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}
type ProjectPolicy struct {
	Enabled         *bool        `json:"enabled,omitempty"`
	AllowedProfiles []ProfileRef `json:"allowedProfiles,omitempty"`
	SelectedProfile *ProfileRef  `json:"selectedProfile,omitempty"`
}
type PolicySnapshot struct {
	Project              ProjectPolicy
	Worker, Orchestrator *bool
}
type CredentialReference struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Version  string `json:"version"`
}
type Deployment struct {
	Profile       Profile             `json:"profile"`
	CredentialRef CredentialReference `json:"credentialRef"`
	Workspace     string              `json:"workspace"`
}
type Registration struct {
	Deployment    Deployment               `json:"deployment"`
	ExternalSpend map[string]ExternalSpend `json:"externalSpend"`
}
type ResolvedProfile struct {
	ID              string `json:"id"`
	Revision        string `json:"revision"`
	BillingProvider string `json:"billing_provider"`
	Model           string `json:"model"`
	Digest          string `json:"digest"`
	Available       bool   `json:"available"`
	Current         bool   `json:"current"`
	PriceVersion    string `json:"priceVersion"`
	Reason          string `json:"reason"`
}
type DelegationView struct {
	ProjectID                  string           `json:"projectId"`
	SessionID                  string           `json:"sessionId"`
	Inputs                     PolicyInputs     `json:"inputs"`
	Source                     string           `json:"source"`
	SessionOverride            string           `json:"sessionOverride"`
	ProjectOptIn               bool             `json:"projectOptIn"`
	EffectiveEnabled           bool             `json:"effectiveEnabled"`
	EmergencyStop              bool             `json:"emergencyStop"`
	Requested                  bool             `json:"requested"`
	Active                     bool             `json:"active"`
	Available                  bool             `json:"available"`
	Reason                     string           `json:"reason"`
	AllowedProfiles            []ProfileRef     `json:"allowedProfiles"`
	SelectedProfile            *ProfileRef      `json:"selectedProfile"`
	ResolvedProfile            *ResolvedProfile `json:"resolvedProfile"`
	SelectionSource            string           `json:"selectionSource"`
	ProjectPolicyRevision      string           `json:"projectPolicyRevision"`
	HostProfileCurrentRevision string           `json:"hostProfileCurrentRevision"`
	ProfileEligibility         string           `json:"profileEligibility"`
}

// sessionOverride is the only per-session state: an explicit on/off choice.
type sessionOverride struct {
	ProjectID string
	Override  *bool
}
type deploymentState struct {
	Version  int
	Stop     bool
	Current  map[string]string
	History  map[string]Deployment
	Sessions map[string]sessionOverride
}
type PolicyRevision struct {
	epoch                  string
	project, session, host uint64
}
type delegationJob struct {
	session, project string
	cancel           context.CancelFunc
}

type Coordinator struct {
	closed                           bool
	root, dir, epoch                 string
	mu                               sync.Mutex
	mutations                        sync.Mutex
	state                            deploymentState
	policies                         map[string]PolicySnapshot
	projectVersions, sessionVersions map[string]uint64
	hostVersion                      uint64
	jobs                             map[string]delegationJob
	loadErr                          error
	budget                           *Budget
	canonical                        Recorder
	provider                         func(Deployment) Transport
	checkpoint                       func(context.Context, string)
}

func NewCoordinator(root string, canonical Recorder, provider func(Deployment) Transport, checkpoint func(context.Context, string)) *Coordinator {
	c := &Coordinator{root: root, dir: filepath.Join(root, "hcg", "delegation"), epoch: rand.Text(), policies: map[string]PolicySnapshot{}, projectVersions: map[string]uint64{}, sessionVersions: map[string]uint64{}, jobs: map[string]delegationJob{}, canonical: canonical, provider: provider, checkpoint: checkpoint}
	c.state = deploymentState{Version: 1, Current: map[string]string{}, History: map[string]Deployment{}, Sessions: map[string]sessionOverride{}}
	c.budget = &Budget{OwnerRoot: root, Dir: filepath.Join(c.dir, "budget")}
	raw, err := os.ReadFile(filepath.Join(c.dir, "registry.json"))
	if err == nil {
		if len(raw) > 4<<20 || json.Unmarshal(raw, &c.state) != nil || c.state.Version != 1 || c.state.Current == nil || c.state.History == nil || c.state.Sessions == nil {
			c.loadErr = ErrUnavailable
		}
	} else if !os.IsNotExist(err) {
		c.loadErr = ErrUnavailable
	}
	if c.loadErr == nil {
		c.loadErr = validateSavedDeployments(c.state)
	}
	return c
}
func (c *Coordinator) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.hostVersion++
	for _, j := range c.jobs {
		j.cancel()
	}
}
func (c *Coordinator) token(id Identity) PolicyRevision {
	return PolicyRevision{c.epoch, c.projectVersions[id.ProjectID], c.sessionVersions[id.SessionID], c.hostVersion}
}
func (c *Coordinator) PolicyRevision(id Identity) PolicyRevision {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token(id)
}
func tokenString(t PolicyRevision) string {
	b, _ := json.Marshal([]any{t.epoch, t.project, t.session, t.host})
	return textHash(string(b))
}
func (c *Coordinator) cancelJobs(project, session string) {
	for _, j := range c.jobs {
		if (project == "" || j.project == project) && (session == "" || j.session == session) {
			j.cancel()
		}
	}
}
func freezePolicy(p PolicySnapshot) PolicySnapshot {
	b, _ := json.Marshal(p)
	var copy PolicySnapshot
	_ = json.Unmarshal(b, &copy)
	return copy
}
func (c *Coordinator) ProjectPolicyChanged(project string, p PolicySnapshot) {
	p = freezePolicy(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	old, known := c.policies[project]
	if !known || !reflect.DeepEqual(old, p) {
		c.policies[project] = p
		c.projectVersions[project]++
		c.cancelJobs(project, "")
	}
}
func (c *Coordinator) edit(ctx context.Context, apply func(*deploymentState) error, project, session string, host bool) error {
	c.mutations.Lock()
	defer c.mutations.Unlock()
	c.mu.Lock()
	if c.loadErr != nil || c.closed {
		c.mu.Unlock()
		return ErrUnavailable
	}
	raw, _ := json.Marshal(c.state)
	c.mu.Unlock()
	var next deploymentState
	if json.Unmarshal(raw, &next) != nil {
		return ErrUnavailable
	}
	if err := apply(&next); err != nil {
		return err
	}
	updated, err := json.Marshal(next)
	if err != nil {
		return ErrUnavailable
	}
	if string(raw) == string(updated) {
		return nil
	}
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return ErrUnavailable
	}
	if err := atomicBudgetFile(ctx, filepath.Join(c.dir, "registry.json"), updated); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var frozen deploymentState
	_ = json.Unmarshal(updated, &frozen)
	c.state = frozen
	if host {
		c.hostVersion++
	} else if session != "" {
		c.sessionVersions[session]++
	} else if project != "" {
		c.projectVersions[project]++
	}
	c.cancelJobs(project, session)
	return nil
}
func validateDeployment(d Deployment) error {
	p := d.Profile
	if !p.valid() || p.DeploymentDigest != "" || d.CredentialRef.Provider != p.BillingProvider || !profileToken.MatchString(d.CredentialRef.Name) || !safeText(d.CredentialRef.Name) || !profileToken.MatchString(d.CredentialRef.Version) || !profileToken.MatchString(d.Workspace) || !safeText(d.Workspace) {
		return ErrUnavailable
	}
	_, err := worstCharge(p)
	return err
}
func validateSavedDeployments(s deploymentState) error {
	for key, d := range s.History {
		original := d.Profile.DeploymentDigest
		digest := d.Profile.digest()
		d.Profile.DeploymentDigest = ""
		if validateDeployment(d) != nil {
			return ErrUnavailable
		}
		raw, _ := json.Marshal(d)
		if original != textHash(string(raw)) || digest != key {
			return ErrUnavailable
		}
	}
	for id, key := range s.Current {
		d, ok := s.History[key]
		if !ok || id != d.Profile.ID {
			return ErrUnavailable
		}
	}
	return nil
}

// Register stores an immutable profile and approves the current budget month.
func (c *Coordinator) Register(ctx context.Context, r Registration) error {
	d := r.Deployment
	// The delegation meter cannot price Luna's long context or cache writes.
	if d.Profile.Model == "gpt-6-luna" {
		return ErrUnavailable
	}
	if err := validateDeployment(d); err != nil {
		return err
	}
	raw, _ := json.Marshal(d)
	d.Profile.DeploymentDigest = textHash(string(raw))
	digest := d.Profile.digest()
	return c.edit(ctx, func(s *deploymentState) error {
		for _, old := range s.History {
			if old.Profile.ID == d.Profile.ID && old.Profile.Revision == d.Profile.Revision && old.Profile.digest() != digest {
				return ErrUnavailable
			}
		}
		for id, key := range s.Current {
			old := s.History[key]
			if old.Profile.BillingProvider == d.Profile.BillingProvider && id != d.Profile.ID {
				return ErrUnavailable
			}
		}
		if err := c.budget.ApprovePeriod(ctx, d.Profile.Prices.Period, r.ExternalSpend); err != nil {
			return err
		}
		s.History[digest] = d
		s.Current[d.Profile.ID] = digest
		return nil
	}, "", "", true)
}
func (c *Coordinator) Profiles() []ResolvedProfile {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := []ResolvedProfile{}
	for digest, d := range c.state.History {
		rows = append(rows, c.profileView(digest, d))
	}
	return rows
}
func (c *Coordinator) profileView(key string, d Deployment) ResolvedProfile {
	available := c.loadErr == nil && c.provider != nil
	reason := "registered"
	if !available {
		reason = "transport_unavailable"
	}
	return ResolvedProfile{ID: d.Profile.ID, Revision: d.Profile.Revision, BillingProvider: d.Profile.BillingProvider, Model: d.Profile.Model, Digest: key, Available: available, Current: c.state.Current[d.Profile.ID] == key, PriceVersion: d.Profile.Prices.Version, Reason: reason}
}
func (c *Coordinator) viewLocked(id Identity, p PolicySnapshot) (DelegationView, Deployment) {
	m := c.state.Sessions[id.SessionID]
	in := PolicyInputs{Project: p.Project.Enabled, Session: m.Override, EmergencyStop: c.state.Stop}
	if id.Role == "worker" {
		in.Role = p.Worker
	} else if id.Role == "orchestrator" {
		in.Role = p.Orchestrator
	}
	resolved := ResolvePolicy(id.Role, in)
	v := DelegationView{ProjectID: id.ProjectID, SessionID: id.SessionID, Inputs: in, Source: resolved.Source, SessionOverride: "inherit", ProjectOptIn: in.Project != nil && *in.Project, EmergencyStop: c.state.Stop, AllowedProfiles: append([]ProfileRef{}, p.Project.AllowedProfiles...), SelectedProfile: p.Project.SelectedProfile, SelectionSource: "project.config", ProjectPolicyRevision: tokenString(c.token(id)), ProfileEligibility: "blocked"}
	if m.Override != nil {
		if *m.Override {
			v.SessionOverride = "on"
		} else {
			v.SessionOverride = "off"
		}
	}
	v.Requested = resolved.Enabled && v.ProjectOptIn
	fail := func(reason string) (DelegationView, Deployment) {
		v.Reason = reason
		v.ProfileEligibility = reason
		return v, Deployment{}
	}
	if c.loadErr != nil || c.closed {
		return fail("registry_unavailable")
	}
	if id.DataRoot != c.root || (id.Role != "worker" && id.Role != "orchestrator") || (id.Harness != "codex" && id.Harness != "claude-code") {
		return fail("unsupported")
	}
	cached, exists := c.policies[id.ProjectID]
	if !exists || !reflect.DeepEqual(cached, p) {
		return fail("policy_snapshot_unavailable")
	}
	if !v.ProjectOptIn {
		return fail("project_opt_in_required")
	}
	if c.state.Stop {
		return fail("emergency_stop")
	}
	if !resolved.Enabled {
		return fail("integration_disabled")
	}
	ref := p.Project.SelectedProfile
	if ref == nil || len(p.Project.AllowedProfiles) == 0 {
		return fail("profile_unset")
	}
	seen := map[string]bool{}
	allowed := false
	for _, candidate := range p.Project.AllowedProfiles {
		if seen[candidate.ID] {
			return fail("profile_ambiguous")
		}
		seen[candidate.ID] = true
		if candidate == *ref {
			allowed = true
		}
	}
	if !allowed {
		return fail("profile_not_allowed")
	}
	key := c.state.Current[ref.ID]
	d, registered := c.state.History[key]
	if !registered {
		return fail("profile_unregistered")
	}
	v.HostProfileCurrentRevision = d.Profile.Revision
	profile := c.profileView(key, d)
	v.ResolvedProfile = &profile
	if d.Profile.Revision != ref.Revision {
		return fail("profile_revision_stale")
	}
	if !profile.Available {
		return fail("profile_unavailable")
	}
	v.Available = true
	v.EffectiveEnabled = true
	v.Active = true
	v.ProfileEligibility = "eligible"
	v.Reason = "ready"
	return v, d
}
func (c *Coordinator) Get(id Identity, p PolicySnapshot) DelegationView {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, _ := c.viewLocked(id, p)
	return v
}
func (c *Coordinator) SetOverride(ctx context.Context, id Identity, enabled *bool) error {
	return c.edit(ctx, func(s *deploymentState) error {
		m := s.Sessions[id.SessionID]
		if m.ProjectID != "" && m.ProjectID != id.ProjectID {
			return ErrUnavailable
		}
		m.ProjectID = id.ProjectID
		if enabled == nil {
			m.Override = nil
		} else {
			copy := *enabled
			m.Override = &copy
		}
		s.Sessions[id.SessionID] = m
		return nil
	}, id.ProjectID, id.SessionID, false)
}
func (c *Coordinator) EmergencyStop(ctx context.Context, enabled bool) error {
	return c.edit(ctx, func(s *deploymentState) error { s.Stop = enabled; return nil }, "", "", true)
}
func (c *Coordinator) Read(ctx context.Context, id Identity, p PolicySnapshot, r Request, revision PolicyRevision) ReadReply {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	c.mu.Lock()
	view, d := c.viewLocked(id, p)
	execution := Execution{ProjectID: id.ProjectID, SessionID: id.SessionID, DelegationID: rand.Text(), AttemptID: rand.Text(), ProjectPolicyRevision: tokenString(revision)}
	if !view.Active || c.token(id) != revision {
		if c.token(id) != revision {
			view.Reason = "policy_revision_changed"
		}
		c.mu.Unlock()
		return failureReply(execution, d.Profile, "unavailable", view.Reason)
	}
	jobID := execution.AttemptID
	c.jobs[jobID] = delegationJob{id.SessionID, id.ProjectID, cancel}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.jobs, jobID); c.mu.Unlock() }()
	if c.checkpoint != nil {
		c.checkpoint(ctx, "admitted")
	}
	if err := ctx.Err(); err != nil {
		return failureReply(execution, d.Profile, "unavailable", contextReason(err))
	}
	scope := ApprovedScope{Name: "session", Revision: scopeRevision, Roots: append([]string{id.Worktree}, id.ReadRoots...)}
	bridge := &budgetBridge{budget: c.budget, execution: execution, profile: d.Profile, canonical: c.canonical, actual: c.provider(d), afterReserve: func(ctx context.Context) {
		if c.checkpoint != nil {
			c.checkpoint(ctx, "reserved")
		}
	},
		commitGuard: func(ctx context.Context, persist func() error) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			v, current := c.viewLocked(id, p)
			if ctx.Err() != nil || !v.Active || c.token(id) != revision || current.Profile.digest() != d.Profile.digest() {
				return ErrUnavailable
			}
			return persist()
		}}
	result, err := (&Consumer{ApprovedScope: scope, Transport: bridge, Recorder: bridge, checkpoint: c.checkpoint}).DelegateRead(ctx, execution, d.Profile, r)
	c.mu.Lock()
	valid := c.token(id) == revision
	c.mu.Unlock()
	if ctx.Err() != nil || !valid {
		return failureReply(execution, d.Profile, "unavailable", contextReason(ctx.Err()))
	}
	if result.Reply.Schema != "" {
		return result.Reply
	}
	if err != nil {
		var scopeErr *ScopeError
		if errors.As(err, &scopeErr) {
			out := failureReply(execution, d.Profile, scopeErr.Status, scopeErr.Error())
			out.Omissions = scopeErr.Omissions
			return out
		}
		reason := "integration_unavailable"
		if errors.Is(err, ErrUsagePending) {
			reason = "usage_pending"
		}
		out := failureReply(execution, d.Profile, "unavailable", reason)
		out.UsageRef.RequestID = result.Receipt.RequestID
		if reason == "usage_pending" {
			out.UsageRef.Status = "usage_pending"
		}
		return out
	}
	return failureReply(execution, d.Profile, "unavailable", "integration_unavailable")
}
func contextReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "integration_unavailable"
}
func failureReply(e Execution, p Profile, status, reason string) ReadReply {
	out := baseReply(e, p, Receipt{UnknownReason: "unavailable", ProfileDigest: p.digest()}, PayloadAudit{Coverage: "none", Sources: []SourceRange{}, Omissions: []Omission{}}, preparedScope{})
	out.Status = status
	out.Reason = reason
	return out
}

// MountRequested is metadata-only: project and role enabled, profile registered
// and available, and no emergency stop.
func (c *Coordinator) MountRequested(id Identity) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id.Mode == "tui" && id.Harness != "codex" {
		return false
	}
	p, known := c.policies[id.ProjectID]
	if !known {
		return false
	}
	v, _ := c.viewLocked(id, p)
	return v.Requested && v.ResolvedProfile != nil && v.ResolvedProfile.Available && p.Project.SelectedProfile != nil && v.ResolvedProfile.Revision == p.Project.SelectedProfile.Revision && v.Reason == "ready"
}

// RecoverReceipts is startup reconciliation only: retry trusted canonical
// writes, never paid requests, current profile resolution or answer delivery.
func (c *Coordinator) RecoverReceipts(ctx context.Context) error {
	state, err := c.checkJournal(ctx)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	c.budget.pending.Range(func(key, value any) bool { keys[key.(string)] = true; return true })
	entries, err := os.ReadDir(c.budget.Dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".pending") {
			key := strings.TrimSuffix(name, ".pending")
			if !validDigest(key) || !entry.Type().IsRegular() {
				return ErrBudgetBlocked
			}
			keys[key] = true
		}
	}
	for key, a := range state.Attempts {
		if a.State == "usage_pending" && !keys[key] {
			return ErrUsagePending
		}
	}
	for key := range keys {
		if _, ok := state.Attempts[key]; !ok {
			return ErrBudgetBlocked
		}
	}
	var first error
	for key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.budget.ReplayReceipt(ctx, key, c.canonical); err != nil && first == nil {
			first = err
		}
	}
	return first
}
