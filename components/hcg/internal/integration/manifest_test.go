package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func boolean(b bool) *bool { return &b }

func testIdentity(root string) Identity {
	return Identity{DataRoot: root, ProjectID: "p1", SessionID: "s1", Role: "worker", Harness: "claude", NativeID: "native-test", LaunchID: "launch-1"}
}

func TestPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, role string
		in         PolicyInputs
		want       Policy
	}{
		{"default", "worker", PolicyInputs{}, Policy{Enabled: false, Source: "default"}},
		{"project", "worker", PolicyInputs{Project: boolean(true)}, Policy{Enabled: true, Source: "project"}},
		{"role off", "worker", PolicyInputs{Project: boolean(true), Role: boolean(false)}, Policy{Enabled: false, Source: "role"}},
		{"session on", "orchestrator", PolicyInputs{Role: boolean(false), Session: boolean(true)}, Policy{Enabled: true, Source: "session"}},
		{"session off", "worker", PolicyInputs{Project: boolean(true), Session: boolean(false)}, Policy{Enabled: false, Source: "session"}},
		{"clear parent on", "worker", PolicyInputs{Project: boolean(true)}, Policy{Enabled: true, Source: "project"}},
		{"clear parent off", "worker", PolicyInputs{Project: boolean(false)}, Policy{Enabled: false, Source: "project"}},
		{"stop", "worker", PolicyInputs{Session: boolean(true), EmergencyStop: true}, Policy{Enabled: false, Source: "emergency_stop"}},
		{"reviewer", "reviewer", PolicyInputs{Project: boolean(true), Session: boolean(true)}, Policy{Enabled: false, Source: "unsupported_role"}},
		{"side", "side", PolicyInputs{Project: boolean(true)}, Policy{Enabled: false, Source: "unsupported_role"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolvePolicy(tc.role, tc.in); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestManifestRestore(t *testing.T) {
	ctx := context.Background()
	id := testIdentity(t.TempDir())
	s := ManifestStore{Root: id.DataRoot}
	if _, err := s.Load(ctx, id); !errors.Is(err, ErrManifestUnavailable) {
		t.Fatalf("missing manifest: %v", err)
	}
	m, err := LaunchManifest(nil, id, PolicyInputs{Project: boolean(true), Session: boolean(false)}, nil)
	if err != nil || m.Requested || m.Active {
		t.Fatalf("initial manifest: %+v %v", m, err)
	}
	if err := s.Save(ctx, m); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Load(ctx, id)
	if err != nil || saved.Inputs.Session == nil || *saved.Inputs.Session {
		t.Fatalf("explicit false not retained: %+v %v", saved, err)
	}
	id.ControllerGeneration = "controller-2"
	live, err := LaunchManifest(&saved, id, PolicyInputs{Project: boolean(false), Session: boolean(true)}, nil)
	if err != nil || live.ConfigDigest != saved.ConfigDigest || live.MountDigest != saved.MountDigest || live.Policy != saved.Policy || live.Identity.ControllerGeneration != id.ControllerGeneration {
		t.Fatalf("live reconnect rewrote historical defaults: %+v %v", live, err)
	}
	id.LaunchID = "launch-2"
	resumed, err := LaunchManifest(&saved, id, PolicyInputs{Project: boolean(true)}, nil)
	if err != nil || resumed.Policy.Enabled || resumed.Policy.Source != "session" {
		t.Fatalf("new launch lost false override: %+v %v", resumed, err)
	}
	saved.Inputs.Session = nil
	resumed, err = LaunchManifest(&saved, id, PolicyInputs{Project: boolean(true)}, nil)
	if err != nil || !resumed.Requested || resumed.Active || resumed.Reason != "engine_unavailable" {
		t.Fatalf("new launch inherit: %+v %v", resumed, err)
	}
	// A newly authorized harness launch retains only the explicit override,
	// never the old native mounting or data source identity.
	changed := id
	changed.Harness, changed.NativeID, changed.LaunchID = "codex", "codex-native", "launch-3"
	saved.Inputs.Session = boolean(false)
	switched, err := LaunchManifest(&saved, changed, PolicyInputs{Project: boolean(true)}, nil)
	if err != nil || switched.Requested || switched.Identity.Harness != "codex" || switched.Identity.NativeID != "codex-native" {
		t.Fatalf("new harness launch: %+v %v", switched, err)
	}
	other := id
	other.DataRoot = t.TempDir()
	if _, err := s.Load(ctx, other); err == nil {
		t.Fatal("cross-root manifest accepted")
	}
	other = id
	other.SessionID = "s2"
	if _, err := s.Load(ctx, other); err == nil {
		t.Fatal("cross-session manifest accepted")
	}
	m.SchemaVersion = 99
	b, _ := json.Marshal(m)
	path := filepath.Join(id.DataRoot, "hcg", "reporting", id.namespace(), "manifest.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, id); err == nil {
		t.Fatal("unknown version accepted")
	}
	if _, err := LaunchManifest(&m, id, PolicyInputs{}, nil); err == nil {
		t.Fatal("unknown version restored")
	}
}
