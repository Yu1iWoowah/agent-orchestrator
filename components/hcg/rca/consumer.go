// Package rca is the named recorded-content consumer of HCG policy/manifest
// mechanics. It has no native launcher, report engine, or built-in disk writer.
package rca

import (
	"context"
	"sync"
	"time"

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
	PurgeNumbers(context.Context, Key) error
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
	reserved bool
}

func (t *Ticket) Context() context.Context { return t.ctx }

type Consumer struct {
	mu        sync.Mutex
	writer    sync.Mutex
	ctx       context.Context
	wg        sync.WaitGroup
	closed    bool
	dirty     map[Key]policy.Manifest
	hostDirty bool
	purges    map[Key]uint64
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
	c := &Consumer{ctx: ctx, dirty: map[Key]policy.Manifest{}, purges: map[Key]uint64{}, store: store, manifests: map[Key]policy.Manifest{}, parents: map[parentKey]policy.Inputs{}, projects: map[parentKey]uint64{}, sessions: map[Key]uint64{}, jobs: map[Key]*Ticket{}}
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

// Current is the memory-only read/publication fence; it performs no storage I/O.
func (c *Consumer) Current(r Revision) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready && !c.closed && r == c.revision(r.key)
}

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

// warm loads policy only while the shared writer is idle. Native admission
// uses already-warmed tickets; it never calls this metadata I/O path.
func (c *Consumer) warm(ctx context.Context, k Key) string {
	if !allowed(k) {
		return "scope_not_allowed"
	}
	c.mu.Lock()
	_, known := c.manifests[k]
	ready := c.ready && !c.closed
	c.mu.Unlock()
	if !ready || ctx.Err() != nil {
		return "storage_unavailable"
	}
	if known {
		return ""
	}
	if !c.writer.TryLock() {
		return "writer_busy"
	}
	defer c.releaseRead()
	m, ok, err := c.store.Load(ctx, k)
	if err != nil || (ok && m.Revision == 0) {
		c.fail()
		return "storage_unavailable"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, known := c.manifests[k]; !known {
		m.Inputs = freeze(m.Inputs)
		c.manifests[k] = m
	}
	return ""
}

// view is memory-only, including cold/unavailable states.
func (c *Consumer) view(ctx context.Context, k Key, in policy.Inputs) (View, string) {
	if !allowed(k) {
		return View{}, "scope_not_allowed"
	}
	if !c.ready || c.closed || ctx.Err() != nil {
		return View{}, "storage_unavailable"
	}
	c.observe(parent(k), in)
	m, known := c.manifests[k]
	if !known {
		return View{}, "writer_busy"
	}
	in = freeze(in)
	in.Session = m.Inputs.Session
	in.EmergencyStop = c.host.Stop
	resolved := policy.Resolve(keyRole(k), in)
	override := "inherit"
	if in.Session != nil {
		override = "off"
		if *in.Session {
			override = "on"
		}
	}
	return View{Inputs: freeze(in), Source: resolved.Source, SessionOverride: override, EffectiveEnabled: resolved.Enabled, EmergencyStop: c.host.Stop, ManifestRevision: m.Revision}, ""
}

// CachedView is the admission/control-plane projection. Cold state is busy;
// no native header check may trigger lazy metadata I/O.
func (c *Consumer) CachedView(ctx context.Context, k Key, in policy.Inputs) (View, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view(ctx, k, in)
}

// Unavailable latches only RCA admission off until explicit storage recovery.
func (c *Consumer) Unavailable() { c.fail() }
func (c *Consumer) View(ctx context.Context, k Key, in policy.Inputs) (View, string) {
	if why := c.warm(ctx, k); why != "" {
		return View{}, why
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view(ctx, k, in)
}
func (c *Consumer) pending() bool { return c.hostDirty || len(c.dirty) > 0 || len(c.purges) > 0 }
func (c *Consumer) fail() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
	for k := range c.jobs {
		c.invalidate(k)
	}
}

// flush owns the sole writer, never the policy mutex during storage calls.
// Dirty entries are latest policy state, not queued projections or retries.
func (c *Consumer) flush() string {
	for {
		c.mu.Lock()
		if !c.ready || c.closed {
			c.mu.Unlock()
			return "storage_unavailable"
		}
		if c.hostDirty {
			h := c.host
			c.mu.Unlock()
			if c.store.SaveHost(c.ctx, h) != nil {
				c.fail()
				return "storage_unavailable"
			}
			c.mu.Lock()
			if c.host.Revision == h.Revision {
				c.hostDirty = false
			}
			c.mu.Unlock()
			continue
		}
		var key Key
		var next policy.Manifest
		found := false
		for k, m := range c.dirty {
			key, next, found = k, m, true
			break
		}
		c.mu.Unlock()
		if !found {
			c.mu.Lock()
			var purgeKey Key
			var purgeRevision uint64
			purgeFound := false
			for k, r := range c.purges {
				purgeKey, purgeRevision, purgeFound = k, r, true
				break
			}
			c.mu.Unlock()
			if !purgeFound {
				return ""
			}
			if c.store.PurgeNumbers(c.ctx, purgeKey) != nil {
				c.fail()
				return "storage_unavailable"
			}
			c.mu.Lock()
			if c.purges[purgeKey] == purgeRevision {
				delete(c.purges, purgeKey)
			}
			c.mu.Unlock()
			continue
		}
		revision := next.Revision
		previous, exists, err := c.store.Load(c.ctx, key)
		if err != nil {
			c.fail()
			return "storage_unavailable"
		}
		if exists && next.Revision <= previous.Revision {
			next.Revision = previous.Revision + 1
		}
		if c.store.Save(c.ctx, key, next) != nil {
			c.fail()
			return "storage_unavailable"
		}
		c.mu.Lock()
		current := c.manifests[key]
		if current.Revision == revision {
			c.manifests[key] = next
			delete(c.dirty, key)
		} else if current.Revision <= next.Revision {
			current.Revision = next.Revision + 1
			c.manifests[key] = current
			c.dirty[key] = current
		}
		c.mu.Unlock()
	}
}

// releaseRead transfers only latest metadata, never queues raw or projections.
// Unlock and the dirty check share the policy lock so a late revocation cannot
// be stranded between the last flush check and writer release.
func (c *Consumer) releaseRead() {
	c.mu.Lock()
	if c.ready && !c.closed && c.pending() {
		c.wg.Add(1)
		c.flushOwned(nil)
	} else {
		c.writer.Unlock()
	}
	c.mu.Unlock()
}
func (c *Consumer) releaseWriter() {
	defer c.releaseRead()
	defer func() {
		if recover() != nil {
			c.fail()
		}
	}()
	_ = c.flush()
}
func (c *Consumer) flushOwned(done chan<- string) {
	go func() {
		why := "storage_unavailable"
		defer func() {
			if recover() != nil {
				c.fail()
				why = "storage_unavailable"
			}
			c.releaseRead()
			c.wg.Done()
			if done != nil {
				done <- why
			}
		}()
		why = c.flush()
	}()
}
func (c *Consumer) persist(ctx context.Context) string {
	if !c.writer.TryLock() {
		return "revocation_persistence_pending"
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.writer.Unlock()
		return "storage_unavailable"
	}
	c.wg.Add(1)
	c.mu.Unlock()
	done := make(chan string, 1)
	c.flushOwned(done)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case why := <-done:
		return why
	case <-ctx.Done():
		return "revocation_persistence_pending"
	case <-timer.C:
		return "revocation_persistence_pending"
	}
}
func (c *Consumer) SetOverride(ctx context.Context, k Key, in policy.Inputs, enabled *bool) (View, string) {
	if !allowed(k) {
		return View{}, "scope_not_allowed"
	}
	wanted := freeze(in)
	wanted.Session = freeze(policy.Inputs{Session: enabled}).Session
	wantsOn := policy.Resolve(keyRole(k), wanted).Enabled
	if wantsOn {
		if why := c.warm(ctx, k); why != "" {
			return View{}, why
		}
	}
	c.mu.Lock()
	if c.closed || !c.ready {
		c.invalidate(k)
		c.mu.Unlock()
		return View{}, "storage_unavailable"
	}
	if wantsOn && c.pending() {
		v, _ := c.view(ctx, k, in)
		c.mu.Unlock()
		return v, "revocation_persistence_pending"
	}
	c.invalidate(k)
	previous := c.manifests[k]
	wanted.EmergencyStop = c.host.Stop
	var before *policy.Manifest
	if previous.Revision > 0 {
		before = &previous
	}
	next := policy.Revise(before, keyRole(k), wanted)
	c.manifests[k] = next
	c.dirty[k] = next
	v, _ := c.view(ctx, k, in)
	c.mu.Unlock()
	why := c.persist(ctx)
	return v, why
}

// Purge withdraws immediately; only its latest deletion state is handed to
// the existing writer, preserving the independent session override.
func (c *Consumer) Purge(ctx context.Context, k Key) string {
	if !allowed(k) {
		return "scope_not_allowed"
	}
	c.mu.Lock()
	c.invalidate(k)
	c.purges[k] = c.sessions[k]
	c.mu.Unlock()
	return c.persist(ctx)
}
func (c *Consumer) EmergencyStop(ctx context.Context, stop bool) string {
	c.mu.Lock()
	if c.closed || !c.ready {
		c.mu.Unlock()
		return "storage_unavailable"
	}
	if !stop && c.pending() {
		c.mu.Unlock()
		return "revocation_persistence_pending"
	}
	for k := range c.jobs {
		c.invalidate(k)
	}
	c.host = HostState{Revision: c.host.Revision + 1, Stop: stop}
	c.hostDirty = true
	c.mu.Unlock()
	return c.persist(ctx)
}
func (c *Consumer) Begin(ctx context.Context, k Key, in policy.Inputs, receipt Revision) (*Ticket, View, string) {
	if why := c.warm(ctx, k); why != "" {
		return nil, View{}, why
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.begin(ctx, k, in, receipt)
}

// BeginCached admits native headers only against already-verified memory.
// It does not warm storage or re-observe a stale launch's policy inputs.
func (c *Consumer) BeginCached(ctx context.Context, k Key, receipt Revision) (*Ticket, View, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if receipt != c.revision(k) {
		return nil, View{}, "policy_changed"
	}
	in, known := c.parents[parent(k)]
	if !known {
		return nil, View{}, "writer_busy"
	}
	return c.begin(ctx, k, in, receipt)
}
func (c *Consumer) begin(ctx context.Context, k Key, in policy.Inputs, receipt Revision) (*Ticket, View, string) {
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
	if c.pending() {
		return nil, v, "revocation_persistence_pending"
	}
	if c.jobs[k] != nil {
		return nil, v, "analysis_running"
	}
	if ctx.Err() != nil {
		return nil, v, "canceled"
	}
	jobCtx, cancel := context.WithCancel(ctx)
	t := &Ticket{key: k, revision: c.revision(k), ctx: jobCtx, cancel: cancel}
	c.jobs[k] = t
	return t, v, ""
}
func (c *Consumer) valid(t *Ticket) bool {
	return t != nil && !c.closed && c.ready && c.jobs[t.key] == t && t.ctx.Err() == nil && t.revision == c.revision(t.key)
}

// Reserve claims the one writer before source/body reads. Revocation cancels
// the ticket but the owner must keep this slot until those reads have stopped.
func (c *Consumer) Reserve(t *Ticket) string {
	if !c.writer.TryLock() {
		return "writer_busy"
	}
	c.mu.Lock()
	why := ""
	if !c.valid(t) {
		why = "policy_changed"
	} else if c.pending() {
		why = "revocation_persistence_pending"
	} else {
		t.reserved = true
	}
	c.mu.Unlock()
	if why != "" {
		c.releaseRead()
	}
	return why
}

// Commit separates slow candidate I/O from publication. publish is a short,
// memory-only callback, called under the final fence even when rejected.
func (c *Consumer) Commit(t *Ticket, write func(context.Context) error, publish func(bool), complete func(context.Context, bool) error) string {
	c.mu.Lock()
	reserved := t != nil && t.reserved
	c.mu.Unlock()
	if !reserved && !c.writer.TryLock() {
		return "writer_busy"
	}
	defer func() {
		c.mu.Lock()
		if t != nil {
			t.reserved = false
		}
		c.mu.Unlock()
		c.releaseWriter()
	}()
	c.mu.Lock()
	valid := c.valid(t)
	c.mu.Unlock()
	if !valid {
		return "policy_changed"
	}
	err := write(t.ctx)
	accepted := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		valid = c.valid(t)
		accepted := valid && err == nil
		if publish != nil {
			publish(accepted)
		}
		return accepted
	}()
	if complete != nil {
		if finishErr := complete(c.ctx, accepted); finishErr != nil {
			err = finishErr
		}
	}
	c.mu.Lock()
	c.finish(t)
	c.mu.Unlock()
	if !valid {
		return "policy_changed"
	}
	if err != nil {
		return "storage_unavailable"
	}
	return ""
}

// WithStorage keeps queries out of a slow writer and uncommitted candidates.
func (c *Consumer) WithStorage(ctx context.Context, read func(context.Context) error) string {
	if !c.writer.TryLock() {
		return "writer_busy"
	}
	defer c.releaseRead()
	c.mu.Lock()
	ready, pending := c.ready && !c.closed, c.pending()
	c.mu.Unlock()
	if !ready {
		return "storage_unavailable"
	}
	if pending {
		return "revocation_persistence_pending"
	}
	if read(ctx) != nil {
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

// Finish is called only after the source/body reader has returned.
func (c *Consumer) Finish(t *Ticket) {
	c.mu.Lock()
	reserved := false
	if t != nil {
		c.finish(t)
		reserved = t.reserved
		t.reserved = false
	}
	c.mu.Unlock()
	if reserved {
		c.releaseRead()
	}
}

// Invalidate fences purge/scope withdrawal without clearing the session override.
func (c *Consumer) Invalidate(k Key) { c.mu.Lock(); defer c.mu.Unlock(); c.invalidate(k) }
func (c *Consumer) Close() {
	c.mu.Lock()
	c.closed = true
	for key := range c.jobs {
		c.invalidate(key)
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// Recover is explicit, serialized storage verification; it never resumes a
// pending revocation or clears the in-process host stop implicitly.
func (c *Consumer) Recover(ctx context.Context, verify func(context.Context) error) string {
	if !c.writer.TryLock() {
		return "revocation_persistence_pending"
	}
	defer c.releaseRead()
	if verify != nil && verify(ctx) != nil {
		return "storage_unavailable"
	}
	h, err := c.store.LoadHost(ctx)
	if err != nil {
		return "storage_unavailable"
	}
	c.mu.Lock()
	h.Stop = h.Stop || c.host.Stop
	if h.Revision < c.host.Revision {
		h.Revision = c.host.Revision
	}
	c.host = h
	c.ready = true
	c.mu.Unlock()
	return c.flush()
}
