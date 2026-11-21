package integration

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
)

type captureFence struct {
	project string
	session string
	version atomic.Uint64
}

// PreparedLaunch is an immutable, in-memory success-boundary snapshot. It
// contains no executable request and cannot be reconstructed from later defaults.
type PreparedLaunch struct {
	manifest             Manifest
	previous             *Manifest
	fence                *captureFence
	version, hostVersion uint64
}

func (c *Consumer) captureFence(id Identity) *captureFence {
	f, _ := c.captures.LoadOrStore(id.namespace(), &captureFence{project: id.ProjectID, session: id.SessionID})
	return f.(*captureFence)
}

func freezeInputs(in PolicyInputs) PolicyInputs {
	copyBool := func(p *bool) *bool {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	in.Project, in.Role, in.Session = copyBool(in.Project), copyBool(in.Role), copyBool(in.Session)
	return in
}

func freezeManifest(m Manifest) Manifest {
	m.Inputs, m.MountInputs = freezeInputs(m.Inputs), freezeInputs(m.MountInputs)
	if m.Engine != nil {
		e := *m.Engine
		m.Engine = &e
	}
	if m.MountEngine != nil {
		e := *m.MountEngine
		m.MountEngine = &e
	}
	return m
}

func (c *Consumer) load(ctx context.Context, id Identity) (Manifest, error) {
	m, err := (ManifestStore{c.root}).Load(ctx, id)
	if err == nil {
		c.snapshots[id.namespace()] = freezeManifest(m)
	} else {
		delete(c.snapshots, id.namespace())
	}
	return m, err
}

func (c *Consumer) save(ctx context.Context, m Manifest) error {
	if err := (ManifestStore{c.root}).Save(ctx, m); err != nil {
		return err
	}
	c.snapshots[m.Namespace] = freezeManifest(m)
	return nil
}

// Prime reads saved overrides at daemon startup, before native launchers run.
func (c *Consumer) Prime(ctx context.Context, id Identity) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.load(ctx, id)
	return err
}

// CaptureLaunch does no I/O and never waits for metadata persistence. If an
// atomic snapshot is not immediately available, only reporting is unavailable.
func (c *Consumer) CaptureLaunch(id Identity, in PolicyInputs, fresh bool) (PreparedLaunch, bool) {
	if id.DataRoot != c.root || !id.valid() || (id.Role != "worker" && id.Role != "orchestrator") || !c.mu.TryLock() {
		return PreparedLaunch{}, false
	}
	defer c.mu.Unlock()
	var previous *Manifest
	if saved, ok := c.snapshots[id.namespace()]; ok {
		m := freezeManifest(saved)
		previous = &m
	}
	if !fresh && previous == nil {
		return PreparedLaunch{}, false
	}
	if !fresh {
		if previous.Identity.Mode != id.Mode || previous.Identity.Harness != id.Harness || (id.NativeID != "" && previous.Identity.NativeID != "" && previous.Identity.NativeID != id.NativeID) {
			return PreparedLaunch{}, false
		}
		// Controller attachment is distinct from the original native launch.
		id.LaunchID = previous.Identity.LaunchID
		in = previous.Inputs
	}
	in = freezeInputs(in)
	in.EmergencyStop = c.host.Stop
	m, err := LaunchManifest(previous, id, in, c.host.Engine)
	if err != nil {
		return PreparedLaunch{}, false
	}
	if fresh && !c.probe.Available {
		m.Active = false
		if m.Policy.Enabled {
			m.Reason = "engine_unavailable"
		}
	}
	f := c.captureFence(id)
	return PreparedLaunch{freezeManifest(m), previous, f, f.version.Load(), c.hostEpoch.Load()}, true
}

func sameLaunch(a, b Identity) bool {
	return a.DataRoot == b.DataRoot && a.ProjectID == b.ProjectID && a.SessionID == b.SessionID && a.Role == b.Role && a.Mode == b.Mode && a.Harness == b.Harness && a.LaunchID == b.LaunchID
}

func sameAttachment(a, b Identity) bool {
	return sameLaunch(a, b) && (a.Mode != "chat" || a.ControllerGeneration == b.ControllerGeneration)
}

func sameOverride(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// StoreLaunch is called by the private metadata worker after re-reading AO's
// committed owner. Old/different owners and policy updates fail closed.
func (c *Consumer) StoreLaunch(ctx context.Context, p PreparedLaunch, authority Identity) error {
	if p.fence == nil || !sameAttachment(p.manifest.Identity, authority) || (p.manifest.Identity.NativeID != "" && authority.NativeID != "" && p.manifest.Identity.NativeID != authority.NativeID) {
		return ErrManifestUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, err := c.load(ctx, authority)
	if err == nil && sameAttachment(current.Identity, authority) && current.MountDigest == p.manifest.MountDigest {
		return nil
	}
	if p.version != p.fence.version.Load() || p.hostVersion != c.hostEpoch.Load() {
		return ErrManifestUnavailable
	}
	if err == nil {
		if p.previous == nil || !sameLaunch(current.Identity, p.previous.Identity) || !sameOverride(current.Inputs.Session, p.manifest.Inputs.Session) {
			return ErrManifestUnavailable
		}
	} else {
		if p.previous != nil {
			return ErrManifestUnavailable
		}
		r, dir, openErr := (ManifestStore{c.root}).open(authority)
		if openErr != nil {
			return openErr
		}
		_, statErr := r.Stat(filepath.Join(dir, "manifest.json"))
		r.Close()
		if !os.IsNotExist(statErr) {
			return ErrManifestUnavailable
		}
	}
	m := freezeManifest(p.manifest)
	m.Identity = authority
	if authority.NativeID == "" {
		m.Active = false
		m.Reason = "native_identity_unknown"
	}
	if err == nil {
		m.Revision = current.Revision + 1
	}
	if err := c.save(ctx, m); err != nil {
		return err
	}
	c.invalidatePolicy(authority.ProjectID, authority.SessionID)
	return nil
}
