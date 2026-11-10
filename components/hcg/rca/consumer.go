// Package rca is the named recorded-content consumer of HCG policy/manifest
// mechanics. It has no native launcher, report engine, or built-in disk writer.
package rca

import (
	"context"
	"sync"

	"github.com/aoagents/agent-orchestrator/components/hcg/policy"
)

type Key struct{ ProjectID, SessionID, Role string }
type parentKey struct{ project, role string }

func keyRole(k Key) string {
	if k.Role == "" {
		return "worker"
	}
	return k.Role
}
func parent(k Key) parentKey { return parentKey{k.ProjectID, keyRole(k)} }

type HostState struct {
	Revision uint64 `json:"revision"`
	Stop     bool   `json:"stop"`
}

// ManifestStore is supplied by AO's RCA storage boundary. Unavailable storage
// fails closed; this consumer does not create a memory-only production fallback.
type ManifestStore interface {
	Load(context.Context, Key) (policy.Manifest, bool, error)
	Save(context.Context, Key, policy.Manifest) error
	LoadHost(context.Context) (HostState, error)
	SaveHost(context.Context, HostState) error
}

type View struct {
	Inputs           policy.Inputs `json:"inputs"`
	Source           string        `json:"source"`
	SessionOverride  string        `json:"sessionOverride"`
	EffectiveEnabled bool          `json:"effectiveEnabled"`
	EmergencyStop    bool          `json:"emergencyStop"`
	ManifestRevision uint64        `json:"manifestRevision"`
}

// Revision must be captured before AO reads policy or source facts. It is an
// opaque admission receipt, not a value a client can submit through the API.
type Revision struct {
	key                    Key
	project, session, host uint64
}
type Ticket struct {
	key      Key
	revision Revision
	ctx      context.Context
	cancel   context.CancelFunc
}

func (t *Ticket) Context() context.Context { return t.ctx }

type Consumer struct {
	mu        sync.Mutex
	store     ManifestStore
	host      HostState
	ready     bool
	manifests map[Key]policy.Manifest
	parents   map[parentKey]policy.Inputs
	projects  map[parentKey]uint64
	sessions  map[Key]uint64
	jobs      map[Key]*Ticket
}

func New(ctx context.Context, store ManifestStore) *Consumer {
	c := &Consumer{store: store, manifests: map[Key]policy.Manifest{}, parents: map[parentKey]policy.Inputs{}, projects: map[parentKey]uint64{}, sessions: map[Key]uint64{}, jobs: map[Key]*Ticket{}}
	if store != nil {
		var err error
		c.host, err = store.LoadHost(ctx)
		c.ready = err == nil
	}
	return c
}

func freeze(in policy.Inputs) policy.Inputs {
	copyBool := func(v *bool) *bool {
		if v == nil {
			return nil
		}
		b := *v
		return &b
	}
	return policy.Inputs{Project: copyBool(in.Project), Role: copyBool(in.Role), Session: copyBool(in.Session), EmergencyStop: in.EmergencyStop}
}
func sameBool(a, b *bool) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func allowed(k Key) bool {
	return k.ProjectID == "agent-orchestrator" && k.SessionID != "" && (keyRole(k) == "worker" || keyRole(k) == "orchestrator")
}
func (c *Consumer) revision(k Key) Revision {
	return Revision{k, c.projects[parent(k)], c.sessions[k], c.host.Revision}
}
func (c *Consumer) Revision(k Key) Revision { c.mu.Lock(); defer c.mu.Unlock(); return c.revision(k) }

func (c *Consumer) invalidate(k Key) {
	c.sessions[k]++
	if job := c.jobs[k]; job != nil {
		job.cancel()
		delete(c.jobs, k)
	}
}

func (c *Consumer) observe(p parentKey, in policy.Inputs) {
	in.Session, in.EmergencyStop = nil, false
	before, known := c.parents[p]
	c.parents[p] = freeze(in)
	if known && (!sameBool(before.Project, in.Project) || !sameBool(before.Role, in.Role)) {
		c.projects[p]++
		for key := range c.jobs {
			if parent(key) == p {
				c.invalidate(key)
			}
		}
	}
}

// Only typed RCA project/role values enter here. Unrelated settings and other
// consumers cannot revoke or enable this consumer's jobs.
func (c *Consumer) ProjectPolicyChanged(project, role string, in policy.Inputs) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observe(parentKey{project, role}, in)
}

func (c *Consumer) view(ctx context.Context, k Key, in policy.Inputs) (View, string) {
	if !allowed(k) {
		return View{}, "scope_not_allowed"
	}
	if !c.ready || ctx.Err() != nil {
		return View{}, "storage_unavailable"
	}
	c.observe(parent(k), in)
	m, loaded := c.manifests[k]
	if !loaded {
		stored, ok, err := c.store.Load(ctx, k)
		if err != nil || (ok && stored.Revision == 0) {
			return View{}, "storage_unavailable"
		}
		if ok {
			m = stored
			m.Inputs = freeze(stored.Inputs)
			c.manifests[k] = m
		}
	}
	in = freeze(in)
	in.Session = m.Inputs.Session
	in.EmergencyStop = c.host.Stop
	p := policy.Resolve(keyRole(k), in)
	override := "inherit"
	if in.Session != nil {
		override = "off"
		if *in.Session {
			override = "on"
		}
	}
	return View{Inputs: freeze(in), Source: p.Source, SessionOverride: override, EffectiveEnabled: p.Enabled, EmergencyStop: c.host.Stop, ManifestRevision: m.Revision}, ""
}

func (c *Consumer) View(ctx context.Context, k Key, in policy.Inputs) (View, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view(ctx, k, in)
}

func (c *Consumer) SetOverride(ctx context.Context, k Key, in policy.Inputs, enabled *bool) (View, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !allowed(k) {
		return View{}, "scope_not_allowed"
	}
	c.invalidate(k)
	if _, why := c.view(ctx, k, in); why != "" {
		return View{}, why
	}
	previous := c.manifests[k]
	in = freeze(in)
	in.Session = freeze(policy.Inputs{Session: enabled}).Session
	in.EmergencyStop = c.host.Stop
	var before *policy.Manifest
	if previous.Revision > 0 {
		before = &previous
	}
	next := policy.Revise(before, keyRole(k), in)
	if c.store.Save(ctx, k, next) != nil {
		return View{}, "storage_unavailable"
	}
	c.manifests[k] = next
	return c.view(ctx, k, in)
}

func (c *Consumer) EmergencyStop(ctx context.Context, stop bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready {
		return "storage_unavailable"
	}
	for key := range c.jobs {
		c.invalidate(key)
	}
	next := HostState{Revision: c.host.Revision + 1, Stop: stop}
	// A failed stop write must still revoke in-process work. Failed unstop
	// leaves the previous stop effective and never implies an opt-in.
	c.host.Revision = next.Revision
	if stop {
		c.host.Stop = true
	}
	if c.store.SaveHost(ctx, next) != nil {
		return "storage_unavailable"
	}
	c.host = next
	return ""
}

func (c *Consumer) Begin(ctx context.Context, k Key, in policy.Inputs, receipt Revision) (*Ticket, View, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, why := c.view(ctx, k, in)
	if why != "" {
		return nil, v, why
	}
	if receipt != c.revision(k) {
		return nil, v, "policy_changed"
	}
	if !v.EffectiveEnabled {
		return nil, v, "disabled"
	}
	if c.jobs[k] != nil {
		return nil, v, "analysis_running"
	}
	if ctx.Err() != nil {
		return nil, v, "canceled"
	}
	jobCtx, cancel := context.WithCancel(ctx)
	t := &Ticket{k, c.revision(k), jobCtx, cancel}
	c.jobs[k] = t
	return t, v, ""
}

// Commit linearizes the final revision check and the safe projection write.
// The callback receives no content and must not reenter this consumer.
func (c *Consumer) Commit(t *Ticket, write func(context.Context) error) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t == nil || c.jobs[t.key] != t || t.ctx.Err() != nil || t.revision != c.revision(t.key) {
		return "policy_changed"
	}
	defer c.finish(t)
	if write(t.ctx) != nil {
		return "storage_unavailable"
	}
	return ""
}

func (c *Consumer) finish(t *Ticket) {
	if c.jobs[t.key] == t {
		delete(c.jobs, t.key)
	}
	t.cancel()
}
func (c *Consumer) Finish(t *Ticket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t != nil {
		c.finish(t)
	}
}

// Invalidate fences purge/scope withdrawal without clearing the session override.
func (c *Consumer) Invalidate(k Key) { c.mu.Lock(); defer c.mu.Unlock(); c.invalidate(k) }
func (c *Consumer) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.jobs {
		c.invalidate(key)
	}
}

// Recover only reloads a previously unavailable metadata boundary after AO has
// explicitly revalidated storage. It never clears an in-process host stop.
func (c *Consumer) Recover(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready {
		return ""
	}
	if c.store == nil {
		return "storage_unavailable"
	}
	h, err := c.store.LoadHost(ctx)
	if err != nil {
		return "storage_unavailable"
	}
	h.Stop = h.Stop || c.host.Stop
	if h.Revision <= c.host.Revision {
		h.Revision = c.host.Revision + 1
	}
	c.host, c.ready = h, true
	return ""
}
