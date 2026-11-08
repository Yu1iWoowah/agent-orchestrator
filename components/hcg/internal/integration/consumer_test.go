package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConsumerPolicyPersistence(t *testing.T) {
	ctx := context.Background()
	id := testIdentity(t.TempDir())
	c, err := NewConsumer(id.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, parent := range []bool{false, true} {
		// Each parent scenario starts a different launch; set/clear never rewrites
		// the mounting of an existing launch.
		id.LaunchID = "parent-off"
		if parent {
			id.LaunchID = "parent-on"
		}
		inputs := PolicyInputs{Project: boolean(parent)}
		if err := c.Launch(ctx, id, inputs); err != nil {
			t.Fatal(err)
		}
		if err := c.SetOverride(ctx, id, inputs, boolean(!parent)); err != nil {
			t.Fatal(err)
		}
		if v := c.Get(ctx, id, inputs); v.EffectiveEnabled == parent || v.Source != "session" {
			t.Fatalf("override lost: %+v", v)
		}
		if err := c.SetOverride(ctx, id, inputs, nil); err != nil {
			t.Fatal(err)
		}
		if v := c.Get(ctx, id, inputs); v.EffectiveEnabled != parent || v.SessionOverride != "inherit" {
			t.Fatalf("clear must inherit actual parent: %+v", v)
		}
		changed := PolicyInputs{Project: boolean(!parent)}
		c.Get(ctx, id, changed)
		saved, err := (ManifestStore{id.DataRoot}).Load(ctx, id)
		if err != nil || saved.Inputs.Project == nil || *saved.Inputs.Project == parent || saved.MountInputs.Project == nil || *saved.MountInputs.Project != parent {
			t.Fatalf("effective snapshot lost launch history: %+v %v", saved, err)
		}
		if err := c.Launch(ctx, id, changed); err != nil {
			t.Fatal(err)
		}
		reconnected, err := (ManifestStore{id.DataRoot}).Load(ctx, id)
		if err != nil || reconnected.MountDigest != saved.MountDigest {
			t.Fatalf("live reconnect changed mounting facts: %+v %v", reconnected, err)
		}
		if err := c.EmergencyStop(ctx, true); err != nil {
			t.Fatal(err)
		}
		if v := c.Get(ctx, id, inputs); v.EffectiveEnabled || !v.EmergencyStop || v.Requested != parent {
			t.Fatalf("stop: %+v", v)
		}
		c, err = NewConsumer(id.DataRoot)
		if err != nil {
			t.Fatal(err)
		}
		if v := c.Get(ctx, id, inputs); !v.EmergencyStop {
			t.Fatal("host stop did not survive restart")
		}
		if err := c.EmergencyStop(ctx, false); err != nil {
			t.Fatal(err)
		}
		if v := c.Get(ctx, id, inputs); v.EffectiveEnabled != parent {
			t.Fatalf("release implicitly enabled: %+v", v)
		}
	}
	other := id
	other.SessionID = "unmounted"
	if v := c.Get(ctx, other, PolicyInputs{Project: boolean(true)}); v.EffectiveEnabled || v.Available || v.Reason != "manifest_unavailable" {
		t.Fatalf("missing manifest guessed defaults: %+v", v)
	}
}

func fixtureEngine(t *testing.T, code string) (string, string) {
	t.Helper()
	_, node, _ := engineTests(t)
	pkg := filepath.Join(t.TempDir(), "node_modules", EngineName)
	if err := os.MkdirAll(filepath.Join(pkg, "dist"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"llm-usage-metrics","version":"0.14.3"}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pkg, "dist", "bin.js")
	code = `if(process.argv.includes('--version')){console.log('0.14.3')}else{` + code + `}`
	if err := os.WriteFile(path, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	return path, node
}

func TestConsumerReportFailures(t *testing.T) {
	for _, tc := range []struct{ name, code, reason string }{
		{"nonzero", `process.exit(7)`, "engine_execution_failed"},
		{"schema", `console.log('{}')`, "event_schema_or_identity_mismatch"},
		{"timeout", `setInterval(()=>{},1000)`, "deadline_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, node := fixtureEngine(t, tc.code)
			id := testIdentity(t.TempDir())
			c, err := NewConsumer(id.DataRoot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Register(t.Context(), path, node, EngineVersion); err != nil {
				t.Fatal(err)
			}
			if err := c.SetOverride(t.Context(), id, PolicyInputs{}, boolean(true)); err != nil {
				t.Fatal(err)
			}
			sourceRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(sourceRoot, "native.jsonl"), []byte(claudeFixture), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			out := c.Report(ctx, id, PolicyInputs{}, []AuthorizedSource{{sourceRoot, "native.jsonl", "claude", id.NativeID}}, c.PolicyRevision(id.ProjectID))
			if out.Status != "unavailable" || out.Reason != tc.reason || len(out.Events) != 0 {
				t.Fatalf("failure not isolated: %+v", out)
			}
			if v := c.Get(t.Context(), id, PolicyInputs{}); !v.EffectiveEnabled {
				t.Fatalf("failure modified policy: %+v", v)
			}
		})
	}
}

func TestStopCancelsOnlyOwnedReports(t *testing.T) {
	for _, cause := range []string{"stop", "project", "override"} {
		t.Run(cause, func(t *testing.T) {
			path, node := fixtureEngine(t, `setInterval(()=>{},1000)`)
			id := testIdentity(t.TempDir())
			c, err := NewConsumer(id.DataRoot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Register(t.Context(), path, node, EngineVersion); err != nil {
				t.Fatal(err)
			}
			if err := c.SetOverride(t.Context(), id, PolicyInputs{}, boolean(true)); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "a.jsonl"), []byte(claudeFixture), 0600); err != nil {
				t.Fatal(err)
			}
			done := make(chan Snapshot, 1)
			go func() {
				done <- c.Report(t.Context(), id, PolicyInputs{}, []AuthorizedSource{{root, "a.jsonl", "claude", id.NativeID}}, c.PolicyRevision(id.ProjectID))
			}()
			deadline := time.Now().Add(3 * time.Second)
			for {
				c.mu.Lock()
				started := len(c.jobs) == 1
				c.mu.Unlock()
				if started {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("report did not start")
				}
				time.Sleep(time.Millisecond)
			}
			switch cause {
			case "stop":
				if err := c.EmergencyStop(t.Context(), true); err != nil {
					t.Fatal(err)
				}
			case "project":
				c.ProjectPolicyChanged("unrelated")
				select {
				case <-done:
					t.Fatal("cancelled another project")
				default:
				}
				c.ProjectPolicyChanged(id.ProjectID)
			case "override":
				if err := c.SetOverride(t.Context(), id, PolicyInputs{}, boolean(false)); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case out := <-done:
				if out.Status != "unavailable" || out.Reason != "cancelled" {
					t.Fatalf("stop leaked late result: %+v", out)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stop blocked behind report")
			}
			if err := c.EmergencyStop(t.Context(), false); err != nil {
				t.Fatal(err)
			}
			if cause == "override" {
				if err := c.SetOverride(t.Context(), id, PolicyInputs{}, boolean(true)); err != nil {
					t.Fatal(err)
				}
			}
			if v := c.Get(t.Context(), id, PolicyInputs{}); !v.EffectiveEnabled {
				t.Fatalf("stop changed override: %+v", v)
			}
			// Changed deployment bytes cannot run under an earlier registration.
			if err := os.WriteFile(path, []byte("process.exit(0)"), 0600); err != nil {
				t.Fatal(err)
			}
			if out := c.Report(t.Context(), id, PolicyInputs{}, []AuthorizedSource{{root, "a.jsonl", "claude", id.NativeID}}, c.PolicyRevision(id.ProjectID)); !strings.Contains(out.Reason, "engine_changed") {
				t.Fatalf("changed binary trusted: %+v", out)
			}

		})
	}
}
