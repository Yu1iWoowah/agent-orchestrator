package rca

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/components/hcg/policy"
)

type fakeManifests struct {
	rows map[Key]policy.Manifest
	host HostState
	fail bool
}

func (s *fakeManifests) Load(_ context.Context, k Key) (policy.Manifest, bool, error) {
	m, ok := s.rows[k]
	return m, ok, nil
}
func (s *fakeManifests) Save(_ context.Context, k Key, m policy.Manifest) error {
	if s.fail {
		return errors.New("synthetic storage failure")
	}
	s.rows[k] = m
	return nil
}
func (s *fakeManifests) LoadHost(context.Context) (HostState, error) { return s.host, nil }
func (s *fakeManifests) SaveHost(_ context.Context, h HostState) error {
	if s.fail {
		return errors.New("synthetic storage failure")
	}
	s.host = h
	return nil
}
func flag(v bool) *bool { return &v }

func TestRCAOverrideClearStopAndABA(t *testing.T) {
	store := &fakeManifests{rows: map[Key]policy.Manifest{}}
	c := New(t.Context(), store)
	k := Key{ProjectID: "agent-orchestrator", SessionID: "selected"}
	in := policy.Inputs{Project: flag(true)}
	if v, why := c.SetOverride(t.Context(), k, in, flag(false)); why != "" || v.EffectiveEnabled || v.SessionOverride != "off" {
		t.Fatalf("explicit false: %+v %s", v, why)
	}
	if v, why := c.SetOverride(t.Context(), k, in, nil); why != "" || !v.EffectiveEnabled || v.SessionOverride != "inherit" {
		t.Fatalf("clear: %+v %s", v, why)
	}
	ticket, _, why := c.Begin(t.Context(), k, in, c.Revision(k))
	if why != "" {
		t.Fatal(why)
	}
	if _, _, why := c.Begin(t.Context(), k, in, c.Revision(k)); why != "analysis_running" {
		t.Fatal("duplicate query admitted another job")
	}
	if _, why := c.SetOverride(t.Context(), k, in, flag(false)); why != "" {
		t.Fatal(why)
	}
	if ticket.Context().Err() == nil {
		t.Fatal("off did not cancel")
	}
	if _, why := c.SetOverride(t.Context(), k, in, flag(true)); why != "" {
		t.Fatal(why)
	}
	writes := 0
	if why := c.Commit(ticket, func(context.Context) error { writes++; return nil }); why != "policy_changed" || writes != 0 {
		t.Fatal("ABA result committed")
	}
	if why := c.EmergencyStop(t.Context(), true); why != "" {
		t.Fatal(why)
	}
	if v, _ := c.View(t.Context(), k, in); v.EffectiveEnabled || !v.EmergencyStop {
		t.Fatal("stop not effective")
	}
	if why := c.EmergencyStop(t.Context(), false); why != "" {
		t.Fatal(why)
	}
	if v, _ := c.View(t.Context(), Key{ProjectID: "agent-orchestrator", SessionID: "other"}, policy.Inputs{}); v.EffectiveEnabled {
		t.Fatal("unstop silently enabled")
	}
}

func TestRCAParentInvalidationFencesAdmissionAndCommit(t *testing.T) {
	c := New(t.Context(), &fakeManifests{rows: map[Key]policy.Manifest{}})
	k := Key{ProjectID: "agent-orchestrator", SessionID: "selected"}
	in := policy.Inputs{Project: flag(true)}
	c.ProjectPolicyChanged(k.ProjectID, "worker", in)
	before := c.Revision(k)
	c.ProjectPolicyChanged(k.ProjectID, "worker", policy.Inputs{Project: flag(false)})
	c.ProjectPolicyChanged(k.ProjectID, "worker", in)
	if _, _, why := c.Begin(t.Context(), k, in, before); why != "policy_changed" {
		t.Fatal("stale pre-read receipt admitted")
	}
	ticket, _, why := c.Begin(t.Context(), k, in, c.Revision(k))
	if why != "" {
		t.Fatal(why)
	}
	c.ProjectPolicyChanged("unrelated", "worker", policy.Inputs{Project: flag(false)})
	if why := c.Commit(ticket, func(context.Context) error { return nil }); why != "" {
		t.Fatal("unrelated project canceled report")
	}
	_, _, why = c.Begin(t.Context(), Key{ProjectID: "another-project", SessionID: "selected"}, in, c.Revision(Key{ProjectID: "another-project", SessionID: "selected"}))
	if why != "scope_not_allowed" {
		t.Fatal("other project admitted")
	}
}

func TestRCAStorageFailureAndPurgeInvalidation(t *testing.T) {
	s := &fakeManifests{rows: map[Key]policy.Manifest{}}
	c := New(t.Context(), s)
	k := Key{ProjectID: "agent-orchestrator", SessionID: "selected"}
	in := policy.Inputs{Project: flag(true)}
	ticket, _, why := c.Begin(t.Context(), k, in, c.Revision(k))
	if why != "" {
		t.Fatal(why)
	}
	s.fail = true
	if _, why := c.SetOverride(t.Context(), k, in, flag(false)); why != "storage_unavailable" {
		t.Fatal("storage error not projected safely")
	}
	if ticket.Context().Err() == nil {
		t.Fatal("failed control left old job running")
	}
	s.fail = false
	ticket, _, why = c.Begin(t.Context(), k, in, c.Revision(k))
	if why != "" {
		t.Fatal(why)
	}
	c.Invalidate(k)
	if why := c.Commit(ticket, func(context.Context) error { t.Fatal("purged job wrote result"); return nil }); why != "policy_changed" {
		t.Fatal(why)
	}
}

func TestRCARoleParentsAreIndependent(t *testing.T) {
	c := New(t.Context(), &fakeManifests{rows: map[Key]policy.Manifest{}})
	worker := Key{ProjectID: "agent-orchestrator", SessionID: "worker", Role: "worker"}
	orchestrator := Key{ProjectID: "agent-orchestrator", SessionID: "orchestrator", Role: "orchestrator"}
	on := policy.Inputs{Project: flag(false), Role: flag(true)}
	off := policy.Inputs{Project: flag(false), Role: flag(false)}
	if _, why := c.View(t.Context(), worker, on); why != "" {
		t.Fatal(why)
	}
	ticket, _, why := c.Begin(t.Context(), worker, on, c.Revision(worker))
	if why != "" {
		t.Fatal(why)
	}
	if view, why := c.View(t.Context(), orchestrator, off); why != "" || view.EffectiveEnabled {
		t.Fatal("orchestrator opt-out ignored", why)
	}
	if ticket.Context().Err() != nil {
		t.Fatal("orchestrator parent replaced/revoked worker parent")
	}
	if why := c.Commit(ticket, func(context.Context) error { return nil }); why != "" {
		t.Fatal(why)
	}
	if _, why := c.View(t.Context(), Key{ProjectID: "agent-orchestrator", SessionID: "reviewer", Role: "reviewer"}, on); why != "scope_not_allowed" {
		t.Fatal("unsupported RCA role admitted")
	}
}
