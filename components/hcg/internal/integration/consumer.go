package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type View struct {
	ProjectID        string       `json:"projectId"`
	SessionID        string       `json:"sessionId"`
	Inputs           PolicyInputs `json:"inputs"`
	Source           string       `json:"source"`
	SessionOverride  string       `json:"sessionOverride"`
	EffectiveEnabled bool         `json:"effectiveEnabled"`
	EmergencyStop    bool         `json:"emergencyStop"`
	Requested        bool         `json:"requested"`
	Active           bool         `json:"active"`
	Available        bool         `json:"available"`
	Reason           string       `json:"reason"`
	EngineVersion    string       `json:"engineVersion"`
	ManifestRevision uint64       `json:"manifestRevision"`
}

type EngineStatus struct {
	Engine    string `json:"engine"`
	Version   string `json:"version"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

type hostState struct {
	SchemaVersion int     `json:"schemaVersion"`
	Stop          bool    `json:"stop"`
	Engine        *Engine `json:"engine"`
}

// Consumer serializes small configuration operations, never subprocess I/O.
// Jobs have cancellation ownership here, not in a native agent/session manager.
type Consumer struct {
	root  string
	mu    sync.Mutex
	host  hostState
	probe EngineStatus
	jobs  map[*reportJob]struct{}
}

type reportJob struct {
	session string
	project string
	cancel  context.CancelFunc
}

// ProjectPolicyChanged is a narrow notification from AO's existing typed
// project-config writer. It never waits for the cancelled external jobs.
func (c *Consumer) ProjectPolicyChanged(project string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for job := range c.jobs {
		if job.project == project {
			job.cancel()
		}
	}
}

func NewConsumer(root string) (*Consumer, error) {
	if !filepath.IsAbs(root) {
		return nil, ErrManifestUnavailable
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, ErrManifestUnavailable
	}
	defer r.Close()
	if err := r.MkdirAll(filepath.Join("hcg", "reporting", "deployment"), 0700); err != nil {
		return nil, ErrManifestUnavailable
	}
	c := &Consumer{root: root, host: hostState{SchemaVersion: 1}, jobs: map[*reportJob]struct{}{}, probe: EngineStatus{Engine: EngineName, Reason: "not_registered"}}
	b, err := r.ReadFile(filepath.Join("hcg", "reporting", "host.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, ErrManifestUnavailable
	}
	if err == nil && (len(b) > 64<<10 || json.Unmarshal(b, &c.host) != nil || c.host.SchemaVersion != 1 || (c.host.Engine != nil && !c.host.Engine.valid())) {
		return nil, ErrManifestUnavailable
	}
	return c, nil
}

func (c *Consumer) runner(id *Identity) Runner {
	dir := "deployment"
	if id != nil {
		dir = id.namespace()
	}
	return Runner{filepath.Join(c.root, "hcg", "reporting", dir)}
}

// Probe is called at daemon startup. It does not install, discover or enable.
func (c *Consumer) Probe(ctx context.Context) EngineStatus {
	c.mu.Lock()
	e := c.host.Engine
	c.mu.Unlock()
	status := EngineStatus{Engine: EngineName, Reason: "not_registered"}
	if e != nil {
		status.Version, status.Reason = e.Version, "engine_unavailable"
		checked, err := c.runner(nil).Register(ctx, e.Executable, e.NodeExecutable, e.Version)
		if err == nil && checked == *e {
			status.Available, status.Reason = true, "ready"
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.host.Engine == e {
		c.probe = status
	}
	return status
}

func (c *Consumer) saveHost(ctx context.Context, state hostState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r, err := os.OpenRoot(c.root)
	if err != nil {
		return ErrManifestUnavailable
	}
	defer r.Close()
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicJSON(ctx, r, filepath.Join("hcg", "reporting", "host.json"), b)
}

func (c *Consumer) cancel(session string) {
	for job := range c.jobs {
		if session == "" || session == job.session {
			job.cancel()
		}
	}
}

func (c *Consumer) Register(ctx context.Context, executable, node, version string) (EngineStatus, error) {
	e, err := c.runner(nil).Register(ctx, executable, node, version)
	if err != nil {
		return EngineStatus{Engine: EngineName, Version: version, Reason: "engine_unavailable"}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.host
	next.Engine = &e
	if err := c.saveHost(ctx, next); err != nil {
		return EngineStatus{}, err
	}
	c.cancel("")
	c.host, c.probe = next, EngineStatus{EngineName, EngineVersion, true, "ready"}
	return c.probe, nil
}

func (c *Consumer) EmergencyStop(ctx context.Context, stop bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.host
	next.Stop = stop
	if stop {
		c.cancel("")
		c.host.Stop = true // A failed persistence write must not reopen reports.
	}
	if err := c.saveHost(ctx, next); err != nil {
		return err
	}
	c.host = next
	return nil
}

func (c *Consumer) Launch(ctx context.Context, id Identity, inputs PolicyInputs) error {
	return c.observeLaunch(ctx, id, inputs, true)
}

func (c *Consumer) Reconnect(ctx context.Context, id Identity, inputs PolicyInputs) error {
	return c.observeLaunch(ctx, id, inputs, false)
}

func (c *Consumer) CancelSessionReports(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel(session)
}

func (c *Consumer) observeLaunch(ctx context.Context, id Identity, inputs PolicyInputs, fresh bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	store := ManifestStore{c.root}
	previous, err := store.Load(ctx, id)
	var old *Manifest
	if err == nil {
		old = &previous
	} else {
		if !fresh {
			return ErrManifestUnavailable
		}
		// Only an absent file permits first launch. Corrupt/unknown state is
		// never overwritten with today's project defaults.
		r, dir, openErr := store.open(id)
		if openErr != nil {
			return openErr
		}
		_, statErr := r.Stat(filepath.Join(dir, "manifest.json"))
		r.Close()
		if !os.IsNotExist(statErr) {
			return ErrManifestUnavailable
		}
	}
	inputs.EmergencyStop = c.host.Stop
	m, err := LaunchManifest(old, id, inputs, c.host.Engine)
	if err != nil {
		return err
	}
	c.cancel(id.SessionID)
	return store.Save(ctx, m)
}

func (c *Consumer) SetOverride(ctx context.Context, id Identity, inputs PolicyInputs, enabled *bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	store := ManifestStore{c.root}
	old, err := store.Load(ctx, id)
	if err != nil {
		r, dir, openErr := store.open(id)
		if openErr != nil {
			return openErr
		}
		_, statErr := r.Stat(filepath.Join(dir, "manifest.json"))
		r.Close()
		if !os.IsNotExist(statErr) {
			return ErrManifestUnavailable
		}
		// Explicit set/clear may create a manifest from AO-owned facts. A
		// passive read/report never takes this initialization path.
		old = Manifest{}
	}
	inputs.Session, inputs.EmergencyStop = enabled, c.host.Stop
	m, err := LaunchManifest(nil, id, inputs, c.host.Engine)
	if err != nil {
		return err
	}
	m.Revision, m.Reason = old.Revision+1, "policy_changed"
	if !m.Active {
		m.Reason = "disabled_or_unavailable"
	}
	c.cancel(id.SessionID)
	return store.Save(ctx, m)
}

func (c *Consumer) effective(ctx context.Context, id Identity, inputs PolicyInputs) (Manifest, View) {
	m, err := (ManifestStore{c.root}).Load(ctx, id)
	v := View{ProjectID: id.ProjectID, SessionID: id.SessionID, Inputs: inputs, SessionOverride: "inherit", EmergencyStop: c.host.Stop, EngineVersion: c.probe.Version, Available: c.probe.Available, Reason: "manifest_unavailable"}
	if err != nil {
		v.Available = false
		return Manifest{}, v
	}
	before := digest(m)
	inputs.Session, inputs.EmergencyStop = m.Inputs.Session, c.host.Stop
	p := ResolvePolicy(id.Role, inputs)
	requested := inputs
	requested.EmergencyStop = false
	v.Inputs, v.Source, v.EffectiveEnabled, v.Requested, v.ManifestRevision = inputs, p.Source, p.Enabled, ResolvePolicy(id.Role, requested).Enabled, m.Revision
	if inputs.Session != nil {
		v.SessionOverride = "off"
		if *inputs.Session {
			v.SessionOverride = "on"
		}
	}
	m.Inputs, m.Policy, m.Engine = inputs, p, c.host.Engine
	// The current AO ownership must still match the recorded mounting.
	if m.Identity.LaunchID != id.LaunchID || m.Identity.Mode != id.Mode || m.Identity.Harness != id.Harness || (m.Identity.NativeID != "" && m.Identity.NativeID != id.NativeID) {
		v.Available, v.Reason = false, "native_identity_unknown"
	} else {
		m.Identity = id
		v.Reason = "disabled"
		if p.Enabled {
			v.Reason = "engine_unavailable"
			if id.NativeID == "" || id.LaunchID == "" {
				v.Available, v.Reason = false, "native_identity_unknown"
			} else if v.Available {
				v.Active, v.Reason = true, "ready"
			}
		}
	}
	m.Active, m.Requested, m.Reason = v.Active, v.Requested, v.Reason
	m.ConfigDigest = configurationDigest(m.Inputs, m.Engine)
	// Persist the effective configuration separately from immutable launch
	// mounting facts; reconnect must not relabel current defaults as history.
	if digest(m) != before {
		m.Revision++
		if err := (ManifestStore{c.root}).Save(ctx, m); err != nil {
			v.EffectiveEnabled, v.Active, v.Available, v.Reason = false, false, false, "manifest_unavailable"
			return Manifest{}, v
		}
	}
	v.ManifestRevision = m.Revision
	return m, v
}

func (c *Consumer) Get(ctx context.Context, id Identity, inputs PolicyInputs) View {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, v := c.effective(ctx, id, inputs)
	return v
}

func (c *Consumer) Report(ctx context.Context, id Identity, inputs PolicyInputs, sources []AuthorizedSource) Snapshot {
	c.mu.Lock()
	m, view := c.effective(ctx, id, inputs)
	if view.Reason == "manifest_unavailable" {
		c.mu.Unlock()
		return Snapshot{Source: "external_analysis", Engine: EngineName, ProjectID: id.ProjectID, SessionID: id.SessionID, Status: "unavailable", Reason: view.Reason, Events: []Event{}, Tools: []ToolCounts{}}
	}
	ctx, cancel := context.WithCancel(ctx)
	job := &reportJob{session: id.SessionID, project: id.ProjectID, cancel: cancel}
	c.jobs[job] = struct{}{}
	c.mu.Unlock()
	out := c.runner(&id).Report(ctx, m, sources)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.jobs, job)
	if out.Reason == "engine_changed_or_unavailable" && c.host.Engine == m.Engine {
		c.probe.Available, c.probe.Reason = false, out.Reason
	}
	if ctx.Err() != nil {
		out.Status, out.Reason, out.Events, out.Tools = "unavailable", reportFailure(ctx, "cancelled"), []Event{}, []ToolCounts{}
	}
	cancel()
	return out
}
