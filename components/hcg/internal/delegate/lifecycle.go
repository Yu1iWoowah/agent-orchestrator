package delegate

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/components/hcg/internal/integration"
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
type InputManifest struct {
	ProjectID string         `json:"projectId"`
	Name      string         `json:"name"`
	Revision  string         `json:"revision"`
	Files     []ApprovedFile `json:"files"`
}
type Installation struct {
	Source          string `json:"source"`
	Venv            string `json:"venv"`
	Commit          string `json:"commit"`
	SourceDigest    string `json:"sourceDigest"`
	RuntimeDigest   string `json:"runtimeDigest"`
	PythonDigest    string `json:"pythonDigest"`
	ConfigDigest    string `json:"configDigest"`
	LockDigest      string `json:"lockDigest"`
	PythonVersion   string `json:"pythonVersion"`
	AdapterVersion  string `json:"adapterVersion"`
	TemplateVersion string `json:"templateVersion"`
}
type Deployment struct {
	AdapterDigest string              `json:"adapterDigest"`
	Profile       Profile             `json:"profile"`
	CredentialRef CredentialReference `json:"credentialRef"`
	Workspace     string              `json:"workspace"`
	Installation  Installation        `json:"installation"`
	Inputs        []InputManifest     `json:"inputs"`
}
type Registration struct {
	Grant         *LabGrant                `json:"grant"`
	Deployment    Deployment               `json:"deployment"`
	ExternalSpend map[string]ExternalSpend `json:"externalSpend"`
}
type ResolvedProfile struct {
	RunID           string `json:"runId"`
	OwnerRoot       string `json:"ownerRoot"`
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
	RunID                      string                   `json:"runId"`
	OwnerRoot                  string                   `json:"ownerRoot"`
	ProjectID                  string                   `json:"projectId"`
	SessionID                  string                   `json:"sessionId"`
	Inputs                     integration.PolicyInputs `json:"inputs"`
	Source                     string                   `json:"source"`
	SessionOverride            string                   `json:"sessionOverride"`
	ProjectOptIn               bool                     `json:"projectOptIn"`
	EffectiveEnabled           bool                     `json:"effectiveEnabled"`
	EmergencyStop              bool                     `json:"emergencyStop"`
	Requested                  bool                     `json:"requested"`
	Active                     bool                     `json:"active"`
	Available                  bool                     `json:"available"`
	Reason                     string                   `json:"reason"`
	AllowedProfiles            []ProfileRef             `json:"allowedProfiles"`
	SelectedProfile            *ProfileRef              `json:"selectedProfile"`
	ResolvedProfile            *ResolvedProfile         `json:"resolvedProfile"`
	SelectionSource            string                   `json:"selectionSource"`
	ProjectPolicyRevision      string                   `json:"projectPolicyRevision"`
	HostProfileCurrentRevision string                   `json:"hostProfileCurrentRevision"`
	ProfileEligibility         string                   `json:"profileEligibility"`
	MountConfigured            bool                     `json:"mountConfigured"`
	MountKnown                 bool                     `json:"mountKnown"`
	MountEnabled               bool                     `json:"mountEnabled"`
	MountProfile               *ProfileRef              `json:"mountProfile"`
	ManifestRevision           uint64                   `json:"manifestRevision"`
}
type sessionMount struct {
	Configured   bool
	Identity     integration.Identity
	Override     *bool
	Known        bool
	MountInputs  integration.PolicyInputs
	MountEnabled bool
	MountProfile *ProfileRef
	Revision     uint64
}
type deploymentState struct {
	Grant    *LabGrant `json:"grant,omitempty"`
	Version  int
	Stop     bool
	Current  map[string]string
	History  map[string]Deployment
	Sessions map[string]sessionMount
}
type PolicyRevision struct {
	epoch                  string
	project, session, host uint64
}
type delegationJob struct {
	session, project string
	cancel           context.CancelFunc
}

// PreparedDelegationLaunch freezes only metadata at the successful boundary.
type PreparedDelegationLaunch struct {
	mount sessionMount
	token PolicyRevision
	fresh bool
	valid bool
}

type Coordinator struct {
	grantReady                       bool
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
	c.state = deploymentState{Version: 1, Current: map[string]string{}, History: map[string]Deployment{}, Sessions: map[string]sessionMount{}}
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
func (c *Coordinator) token(id integration.Identity) PolicyRevision {
	return PolicyRevision{c.epoch, c.projectVersions[id.ProjectID], c.sessionVersions[id.SessionID], c.hostVersion}
}
func (c *Coordinator) PolicyRevision(id integration.Identity) PolicyRevision {
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
func (c *Coordinator) Register(ctx context.Context, r Registration) error {
	d := r.Deployment
	if !r.Grant.valid(c.root) || !r.Grant.current(c.budget.now()) || r.Grant.Profiles[d.Profile.BillingProvider] != (ProfileRef{ID: d.Profile.ID, Revision: d.Profile.Revision}) {
		return ErrUnavailable
	}
	if err := validateDeployment(d); err != nil {
		return err
	}
	d.AdapterDigest = textHash(string(palBridge))
	d.Profile.DeploymentDigest = ""
	raw, _ := json.Marshal(d)
	d.Profile.DeploymentDigest = textHash(string(raw))
	digest := d.Profile.digest()
	if err := verifyInstallation(ctx, d.Installation); err != nil {
		return err
	}
	err := c.edit(ctx, func(s *deploymentState) error {
		grant, err := mergeLabGrant(s.Grant, r.Grant)
		if err != nil {
			return err
		}
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
		// edit holds the registration mutation lock. Reject all immutable
		// conflicts before recovery or approval can commit the budget grant.
		if err := c.prepareRegistration(ctx); err != nil {
			return err
		}
		if err := c.budget.ApproveLabRun(ctx, r.Grant, d.Profile.Prices.Period, r.ExternalSpend); err != nil {
			return err
		}
		s.Grant = grant
		s.History[digest] = d
		s.Current[d.Profile.ID] = digest
		return nil
	}, "", "", true)
	if err == nil {
		c.mu.Lock()
		if !c.grantReady {
			c.hostVersion++
			c.cancelJobs("", "")
		}
		c.grantReady = true
		c.mu.Unlock()
	}
	return err
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
	available := c.grantReady && c.state.Grant.current(c.budget.now()) && c.state.Grant.Profiles[d.Profile.BillingProvider] == (ProfileRef{ID: d.Profile.ID, Revision: d.Profile.Revision}) && c.loadErr == nil && c.provider != nil && runtime.GOOS == "linux" && d.AdapterDigest == textHash(string(palBridge))
	reason := "registered"
	if !available {
		reason = "transport_or_isolation_unavailable"
	}
	run, root := "", ""
	if c.state.Grant != nil {
		run, root = c.state.Grant.RunID, c.state.Grant.OwnerRoot
	}
	return ResolvedProfile{RunID: run, OwnerRoot: root, ID: d.Profile.ID, Revision: d.Profile.Revision, BillingProvider: d.Profile.BillingProvider, Model: d.Profile.Model, Digest: key, Available: available, Current: c.state.Current[d.Profile.ID] == key, PriceVersion: d.Profile.Prices.Version, Reason: reason}
}
func (c *Coordinator) viewLocked(id integration.Identity, p PolicySnapshot) (DelegationView, Deployment) {
	m, known := c.state.Sessions[id.SessionID]
	in := integration.PolicyInputs{Project: p.Project.Enabled, Session: m.Override, EmergencyStop: c.state.Stop}
	if id.Role == "worker" {
		in.Role = p.Worker
	} else if id.Role == "orchestrator" {
		in.Role = p.Orchestrator
	}
	resolved := integration.ResolvePolicy(id.Role, in)
	v := DelegationView{ProjectID: id.ProjectID, SessionID: id.SessionID, Inputs: in, Source: resolved.Source, SessionOverride: "inherit", ProjectOptIn: in.Project != nil && *in.Project, EmergencyStop: c.state.Stop, AllowedProfiles: append([]ProfileRef{}, p.Project.AllowedProfiles...), SelectedProfile: p.Project.SelectedProfile, SelectionSource: "project.config", ProjectPolicyRevision: tokenString(c.token(id)), ProfileEligibility: "blocked", MountConfigured: m.Configured, MountKnown: known && m.Known, MountEnabled: m.MountEnabled, MountProfile: m.MountProfile, ManifestRevision: m.Revision}
	if c.state.Grant != nil {
		v.RunID, v.OwnerRoot = c.state.Grant.RunID, c.state.Grant.OwnerRoot
	}
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
	if !c.grantReady || !c.state.Grant.current(c.budget.now()) || !c.state.Grant.bound(id.ProjectID, id.SessionID) {
		return fail("lab_authorization_unavailable")
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
	if !v.MountKnown || m.Identity.DataRoot != id.DataRoot || m.Identity.Role != id.Role || m.Identity.Worktree != id.Worktree || (id.Mode == "chat" && m.Identity.ControllerGeneration != id.ControllerGeneration) || m.Identity.ProjectID != id.ProjectID || m.Identity.LaunchID != id.LaunchID || m.Identity.Mode != id.Mode || m.Identity.Harness != id.Harness || (m.Identity.NativeID != "" && id.NativeID != m.Identity.NativeID) {
		return fail("manifest_unavailable")
	}
	if id.NativeID == "" {
		return fail("native_identity_unknown")
	}
	v.Available = true
	v.EffectiveEnabled = true
	if !m.Configured {
		v.Reason = "pending_next_safe_launch"
		v.ProfileEligibility = "eligible"
		return v, d
	}
	v.Active = true
	v.ProfileEligibility = "eligible"
	v.Reason = "ready"
	return v, d
}
func (c *Coordinator) Get(id integration.Identity, p PolicySnapshot) DelegationView {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, _ := c.viewLocked(id, p)
	return v
}
func (c *Coordinator) SetOverride(ctx context.Context, id integration.Identity, enabled *bool) error {
	return c.edit(ctx, func(s *deploymentState) error {
		m := s.Sessions[id.SessionID]
		if m.Identity.SessionID != "" && (m.Identity.ProjectID != id.ProjectID || m.Identity.SessionID != id.SessionID) {
			return ErrUnavailable
		}
		if m.Identity.SessionID == "" {
			m.Identity = id
		}
		if enabled == nil {
			m.Override = nil
		} else {
			copy := *enabled
			m.Override = &copy
		}
		m.Revision++
		s.Sessions[id.SessionID] = m
		return nil
	}, id.ProjectID, id.SessionID, false)
}
func (c *Coordinator) EmergencyStop(ctx context.Context, enabled bool) error {
	return c.edit(ctx, func(s *deploymentState) error { s.Stop = enabled; return nil }, "", "", true)
}
func (c *Coordinator) CaptureLaunch(id integration.Identity, fresh bool, configured bool) (PreparedDelegationLaunch, bool) {
	if id.DataRoot != c.root || id.SessionID == "" || (id.Role != "worker" && id.Role != "orchestrator") || !c.mu.TryLock() {
		return PreparedDelegationLaunch{}, false
	}
	defer c.mu.Unlock()
	p, known := c.policies[id.ProjectID]
	if !known {
		return PreparedDelegationLaunch{}, false
	}
	old, exists := c.state.Sessions[id.SessionID]
	if !fresh && (!exists || !old.Known) {
		return PreparedDelegationLaunch{}, false
	}
	if exists && old.Known && (!fresh || (old.Identity.LaunchID == id.LaunchID && id.LaunchID != "")) {
		if old.Identity.Harness != id.Harness || old.Identity.Mode != id.Mode || (id.NativeID != "" && old.Identity.NativeID != "" && id.NativeID != old.Identity.NativeID) {
			return PreparedDelegationLaunch{}, false
		}
		old.Identity.ControllerGeneration = id.ControllerGeneration
		return PreparedDelegationLaunch{mount: old, token: c.token(id), fresh: fresh, valid: true}, true
	}
	in := integration.PolicyInputs{Project: p.Project.Enabled, Session: old.Override, EmergencyStop: c.state.Stop}
	if id.Role == "worker" {
		in.Role = p.Worker
	} else {
		in.Role = p.Orchestrator
	}
	policy := integration.ResolvePolicy(id.Role, in)
	m := sessionMount{Configured: configured, Identity: id, Override: old.Override, Known: true, MountInputs: in, MountEnabled: policy.Enabled && in.Project != nil && *in.Project, MountProfile: p.Project.SelectedProfile, Revision: old.Revision + 1}
	return PreparedDelegationLaunch{mount: m, token: c.token(id), fresh: fresh, valid: true}, true
}
func (c *Coordinator) StoreLaunch(ctx context.Context, p PreparedDelegationLaunch, id integration.Identity) error {
	if !p.valid || p.mount.Identity.ProjectID != id.ProjectID || p.mount.Identity.SessionID != id.SessionID || p.mount.Identity.Mode != id.Mode || p.mount.Identity.Harness != id.Harness || p.mount.Identity.LaunchID != id.LaunchID || p.mount.Identity.ControllerGeneration != id.ControllerGeneration {
		return ErrUnavailable
	}
	return c.edit(ctx, func(s *deploymentState) error {
		c.mu.Lock()
		valid := c.token(id) == p.token
		c.mu.Unlock()
		if !valid {
			return ErrUnavailable
		}
		m := p.mount
		m.Identity = id
		s.Sessions[id.SessionID] = m
		return nil
	}, id.ProjectID, id.SessionID, false)
}
func (c *Coordinator) Read(ctx context.Context, id integration.Identity, p PolicySnapshot, r Request, revision PolicyRevision) ReadReply {
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
	if c.state.Grant != nil {
		bounded, end := context.WithDeadline(ctx, c.state.Grant.ExpiresAt)
		defer end()
		ctx = bounded
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
	if err := os.MkdirAll(filepath.Join(c.dir, "jobs"), 0700); err != nil {
		return failureReply(execution, d.Profile, "unavailable", "integration_unavailable")
	}
	jobDir, err := os.MkdirTemp(filepath.Join(c.dir, "jobs"), "d-")
	if err != nil {
		return failureReply(execution, d.Profile, "unavailable", "integration_unavailable")
	}
	defer os.RemoveAll(jobDir)
	pal, err := stageInstallation(ctx, jobDir, d.Installation)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return failureReply(execution, d.Profile, "unavailable", contextReason(err))
	}
	var scope ApprovedScope
	for _, input := range d.Inputs {
		if input.ProjectID == id.ProjectID {
			scope = ApprovedScope{Name: input.Name, Revision: input.Revision, Root: id.Worktree, Files: input.Files}
		}
	}
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
	result, err := (&Consumer{ApprovedScope: scope, PAL: pal, Transport: bridge, Recorder: bridge, checkpoint: c.checkpoint}).DelegateRead(ctx, execution, d.Profile, r)
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

// MountRequested is metadata-only; it neither grants permission nor starts PAL.
func (c *Coordinator) MountRequested(id integration.Identity) bool {
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
	return v.Requested && v.ResolvedProfile != nil && v.ResolvedProfile.Available && p.Project.SelectedProfile != nil && v.ResolvedProfile.Revision == p.Project.SelectedProfile.Revision && (v.Reason == "ready" || v.Reason == "manifest_unavailable" || v.Reason == "native_identity_unknown" || v.Reason == "pending_next_safe_launch")
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
