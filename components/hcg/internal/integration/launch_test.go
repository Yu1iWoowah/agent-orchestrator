package integration

import (
	"context"
	"testing"
	"time"
)

func TestCapturedLaunchFreezesPolicyAndIsIdempotent(t *testing.T) {
	id := testIdentity(t.TempDir())
	id.Mode, id.ControllerGeneration = "chat", "controller-1"
	c, err := NewConsumer(id.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	project := true
	p, ok := c.CaptureLaunch(id, PolicyInputs{Project: &project}, true)
	if !ok {
		t.Fatal("fresh snapshot unavailable")
	}
	project = false
	if err := c.StoreLaunch(t.Context(), p, id); err != nil {
		t.Fatal(err)
	}
	m, err := (ManifestStore{id.DataRoot}).Load(t.Context(), id)
	if err != nil || m.MountInputs.Project == nil || !*m.MountInputs.Project {
		t.Fatalf("queued snapshot read later defaults: %+v %v", m, err)
	}
	if err := c.StoreLaunch(t.Context(), p, id); err != nil {
		t.Fatal(err)
	}
	duplicate, _ := (ManifestStore{id.DataRoot}).Load(t.Context(), id)
	if duplicate.Revision != m.Revision || duplicate.MountDigest != m.MountDigest {
		t.Fatal("duplicate rewrote the launch")
	}
	live := id
	live.ControllerGeneration = "controller-2"
	if view := c.Get(t.Context(), live, PolicyInputs{Project: boolean(true)}); view.Available || view.Active || view.Reason != "native_identity_unknown" {
		t.Fatalf("old controller treated as active before notification: %+v", view)
	}
	attachment, ok := c.CaptureLaunch(live, PolicyInputs{Project: boolean(false)}, false)
	if !ok {
		t.Fatal("live snapshot unavailable")
	}
	if err := c.StoreLaunch(t.Context(), attachment, live); err != nil {
		t.Fatal(err)
	}
	after, _ := (ManifestStore{id.DataRoot}).Load(t.Context(), id)
	if after.MountDigest != m.MountDigest || after.Identity.LaunchID != id.LaunchID || after.Identity.ControllerGeneration != live.ControllerGeneration {
		t.Fatalf("live reconnect changed mounting: %+v", after)
	}
	// A delayed old attachment cannot replace the current controller facts.
	if err := c.StoreLaunch(t.Context(), p, live); err == nil {
		t.Fatal("late old owner accepted")
	}
}

func TestCapturedLaunchFailsClosedWithoutBlocking(t *testing.T) {
	id := testIdentity(t.TempDir())
	id.Mode, id.ControllerGeneration = "chat", "controller-1"
	c, err := NewConsumer(id.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.CaptureLaunch(id, PolicyInputs{Project: boolean(true)}, false); ok {
		t.Fatal("live reconnect invented a missing manifest")
	}
	c.mu.Lock()
	start := time.Now()
	_, ok := c.CaptureLaunch(id, PolicyInputs{}, true)
	c.mu.Unlock()
	if ok || time.Since(start) > time.Second {
		t.Fatal("notification waited for metadata I/O lock")
	}
	p, ok := c.CaptureLaunch(id, PolicyInputs{Project: boolean(true)}, true)
	if !ok {
		t.Fatal("capture failed")
	}
	newOwner := id
	newOwner.LaunchID, newOwner.ControllerGeneration = "launch-2", "controller-2"
	if err := c.StoreLaunch(context.Background(), p, newOwner); err == nil {
		t.Fatal("late launch overwrote a new owner")
	}
	if _, err := (ManifestStore{id.DataRoot}).Load(t.Context(), id); err == nil {
		t.Fatal("rejected launch created a manifest")
	}
}
