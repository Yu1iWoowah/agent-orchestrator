package integration

import (
	"reflect"
	"strings"
	"testing"
)

func TestOverridePreservesExistingLaunchMount(t *testing.T) {
	id := testIdentity(t.TempDir())
	c, err := NewConsumer(id.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Launch(t.Context(), id, PolicyInputs{Project: boolean(true)}); err != nil {
		t.Fatal(err)
	}
	original, err := (ManifestStore{id.DataRoot}).Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// A later deployment and defaults change are current settings, not history.
	c.host.Engine = &Engine{Executable: "/new/dist/bin.js", NodeExecutable: "/new/node", Version: EngineVersion, Digest: strings.Repeat("a", 64)}
	changed := PolicyInputs{Project: boolean(false)}
	c.Get(t.Context(), id, changed)
	revision := original.Revision
	for _, override := range []*bool{boolean(true), boolean(false), nil} {
		if err := c.SetOverride(t.Context(), id, changed, override); err != nil {
			t.Fatal(err)
		}
		got, err := (ManifestStore{id.DataRoot}).Load(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.MountInputs, original.MountInputs) || !reflect.DeepEqual(got.MountPolicy, original.MountPolicy) || !reflect.DeepEqual(got.MountEngine, original.MountEngine) || got.MountDigest != original.MountDigest {
			t.Errorf("set/clear rewrote original Mount facts: %+v", got)
		}
		if !sameOverride(got.Inputs.Session, override) || got.Policy.Enabled != (override != nil && *override) || got.Revision <= revision {
			t.Errorf("current override/effective/revision not updated: %+v", got)
		}
		revision = got.Revision
	}
	if err := c.Reconnect(t.Context(), id, changed); err != nil {
		t.Fatal(err)
	}
	live, err := (ManifestStore{id.DataRoot}).Load(t.Context(), id)
	if err != nil || live.MountDigest != original.MountDigest {
		t.Fatalf("live lost original Mount: %+v %v", live, err)
	}
	newOwner := id
	newOwner.LaunchID = "different-launch"
	prepared, ok := c.CaptureLaunch(newOwner, changed, true)
	if !ok {
		t.Fatal("fresh boundary snapshot unavailable")
	}
	if err := c.SetOverride(t.Context(), newOwner, changed, boolean(true)); err == nil {
		t.Fatal("policy edit fabricated another launch mounting")
	}
	untouched, err := (ManifestStore{id.DataRoot}).Load(t.Context(), id)
	if err != nil || untouched.Identity.LaunchID != id.LaunchID || untouched.MountDigest != original.MountDigest {
		t.Fatalf("failed policy edit changed original mounting: %+v %v", untouched, err)
	}

	if err := c.StoreLaunch(t.Context(), prepared, newOwner); err != nil {
		t.Fatalf("failed policy edit invalidated successful launch snapshot: %v", err)
	}

}
