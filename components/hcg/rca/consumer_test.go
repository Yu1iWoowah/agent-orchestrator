package rca

import (
	"context"
	"errors"
	"testing"
	"time"

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
func (s *fakeManifests) PurgeNumbers(context.Context, Key) error { return nil }
func flag(v bool) *bool                                          { return &v }

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
	if why := c.Commit(ticket, func(context.Context) error { writes++; return nil }, nil, nil); why != "policy_changed" || writes != 0 {
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
	if why := c.Commit(ticket, func(context.Context) error { return nil }, nil, nil); why != "" {
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
	if _, _, why := c.Begin(t.Context(), k, in, c.Revision(k)); why != "storage_unavailable" {
		t.Fatal("storage failure implicitly recovered")
	}
	if why := c.Recover(t.Context(), nil); why != "" {
		t.Fatal(why)
	}
	if _, why := c.SetOverride(t.Context(), k, in, flag(true)); why != "" {
		t.Fatal(why)
	}
	ticket, _, why = c.Begin(t.Context(), k, in, c.Revision(k))
	if why != "" {
		t.Fatal(why)
	}
	c.Invalidate(k)
	if why := c.Commit(ticket, func(context.Context) error { t.Fatal("purged job wrote result"); return nil }, nil, nil); why != "policy_changed" {
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
	if why := c.Commit(ticket, func(context.Context) error { return nil }, nil, nil); why != "" {
		t.Fatal(why)
	}
	if _, why := c.View(t.Context(), Key{ProjectID: "agent-orchestrator", SessionID: "reviewer", Role: "reviewer"}, on); why != "scope_not_allowed" {
		t.Fatal("unsupported RCA role admitted")
	}
}

func TestRCARevocationDoesNotWaitForSlowCommit(t *testing.T) {
	c := New(t.Context(), &fakeManifests{rows: map[Key]policy.Manifest{}})
	k := Key{ProjectID: "agent-orchestrator", SessionID: "selected"}
	in := policy.Inputs{Project: flag(true)}
	ticket, _, why := c.Begin(t.Context(), k, in, c.Revision(k))
	if why != "" {
		t.Fatal(why)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	committed := make(chan string, 1)
	go func() {
		committed <- c.Commit(ticket, func(context.Context) error { close(entered); <-release; return nil }, nil, nil)
	}()
	<-entered
	controlled := make(chan string, 1)
	go func() { _, why := c.SetOverride(t.Context(), k, in, flag(false)); controlled <- why }()
	waitControl := false
	select {
	case why := <-controlled:
		if why != "revocation_persistence_pending" {
			t.Errorf("unconfirmed durability reported as %q", why)
		}
		if ticket.Context().Err() == nil {
			t.Error("off did not revoke in-memory ticket")
		}
	case <-time.After(2 * time.Second):
		t.Error("off waited for the blocked disk write")
		waitControl = true
	}
	close(release)
	if waitControl {
		<-controlled
	}
	if why := <-committed; why != "policy_changed" {
		t.Errorf("late candidate published: %q", why)
	}
}

func TestRCAApprovedCommitLinearizesBeforeRevocation(t *testing.T) {
	c := New(t.Context(), &fakeManifests{rows: map[Key]policy.Manifest{}})
	k := Key{ProjectID: "agent-orchestrator", SessionID: "selected"}
	in := policy.Inputs{Project: flag(true)}
	ticket, _, why := c.Begin(t.Context(), k, in, c.Revision(k))
	if why != "" {
		t.Fatal(why)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	published := 0
	done := make(chan string, 1)
	go func() {
		done <- c.Commit(ticket, func(context.Context) error { return nil }, func(accepted bool) {
			if accepted {
				published++
			}
		}, func(_ context.Context, accepted bool) error {
			if !accepted {
				return errors.New("unexpected rejection")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	_, why = c.SetOverride(t.Context(), k, in, flag(false))
	close(release)
	if why != "revocation_persistence_pending" || published != 1 {
		t.Fatal("commit-before-off order lost")
	}
	if why := <-done; why != "" {
		t.Fatal(why)
	}
	if view, why := c.View(t.Context(), k, in); why != "" || view.EffectiveEnabled {
		t.Fatal("late I/O revived off policy")
	}
}

func TestRCAReservedAdmissionKeepsSlotUntilReaderStops(t *testing.T) {
	c := New(t.Context(), &fakeManifests{rows: map[Key]policy.Manifest{}})
	k := Key{ProjectID: "agent-orchestrator", SessionID: "selected"}
	in := policy.Inputs{Project: flag(true)}
	ticket, _, why := c.Begin(t.Context(), k, in, c.Revision(k))
	if why != "" {
		t.Fatal(why)
	}
	if why := c.Reserve(ticket); why != "" {
		t.Fatal(why)
	}
	reads := 0
	if why := c.WithStorage(t.Context(), func(context.Context) error { reads++; return nil }); why != "writer_busy" || reads != 0 {
		t.Fatal("reserved admission allowed competing I/O")
	}
	if _, why := c.SetOverride(t.Context(), k, in, flag(false)); why != "revocation_persistence_pending" {
		t.Fatal(why)
	}
	if ticket.Context().Err() == nil {
		t.Fatal("reader not cancelled")
	}
	if why := c.WithStorage(t.Context(), func(context.Context) error { reads++; return nil }); why != "writer_busy" || reads != 0 {
		t.Fatal("cancel released slot before reader stopped")
	}
	c.Finish(ticket)
	c.Close()
}

func TestRCAOtherRolePendingDoesNotRevokeReservedCommit(t *testing.T) {
	c := New(t.Context(), &fakeManifests{rows: map[Key]policy.Manifest{}})
	worker := Key{ProjectID: "agent-orchestrator", SessionID: "worker", Role: "worker"}
	orchestrator := Key{ProjectID: "agent-orchestrator", SessionID: "orchestrator", Role: "orchestrator"}
	in := policy.Inputs{Project: flag(true)}
	if _, why := c.View(t.Context(), orchestrator, in); why != "" {
		t.Fatal(why)
	}
	ticket, _, why := c.Begin(t.Context(), worker, in, c.Revision(worker))
	if why != "" {
		t.Fatal(why)
	}
	if why := c.Reserve(ticket); why != "" {
		t.Fatal(why)
	}
	if _, why := c.SetOverride(t.Context(), orchestrator, in, flag(false)); why != "revocation_persistence_pending" {
		t.Fatal(why)
	}
	if ticket.Context().Err() != nil {
		t.Fatal("other role revoked worker")
	}
	if why := c.Commit(ticket, func(context.Context) error { return nil }, nil, nil); why != "" {
		t.Fatal("other role pending metadata revoked approved worker:", why)
	}
}
