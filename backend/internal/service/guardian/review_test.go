package guardian

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func (f *fixture) chat() {
	f.t.Helper()
	_, err := f.db.Exec("UPDATE sessions SET session_mode='chat',provider_conversation_id='root',controller_generation='generation' WHERE id=?", f.session.ID)
	must(f.t, err)
}
func (f *fixture) review(id, action string, done bool) domain.GuardianReview {
	start := f.base.Add(time.Minute)
	end := start.Add(time.Second)
	r := domain.GuardianReview{ReviewID: id, ParentThreadID: "root", ParentTurnID: "turn", Action: action, Status: "inProgress", StartedAt: &start, SawStarted: !done, SawCompleted: done}
	if done {
		r.Status = "approved"
		r.DecisionSource = "agent"
		r.CompletedAt = &end
	}
	return r
}
func (f *fixture) recordReview(r domain.GuardianReview) {
	f.t.Helper()
	must(f.t, f.store.RecordGuardianReview(context.Background(), f.reviewScope("generation"), r, f.now))
}
func TestA2ReviewLifecycleOrderingReplayAndTarget(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.chat()
	completed := f.review("network", "networkAccess", true)
	f.recordReview(completed)
	f.recordReview(f.review("network", "networkAccess", false))
	var first, timeBefore time.Time
	must(t, f.db.QueryRow("SELECT first_seen_at,retention_at FROM guardian_reviews WHERE review_id='network'").Scan(&first, &timeBefore))
	f.now = f.now.Add(time.Hour)
	f.recordReview(completed)
	var status string
	var sawStart, sawDone, conflict int
	var target *string
	var after, timeAfter time.Time
	must(t, f.db.QueryRow("SELECT status,saw_started,saw_completed,conflict,target_item_id,first_seen_at,retention_at FROM guardian_reviews WHERE review_id='network'").Scan(&status, &sawStart, &sawDone, &conflict, &target, &after, &timeAfter))
	if status != "approved" || sawStart != 1 || sawDone != 1 || conflict != 0 || target != nil || !first.Equal(after) || !timeBefore.Equal(timeAfter) {
		t.Fatalf("bad lifecycle: %s %d %d %d %v", status, sawStart, sawDone, conflict, target)
	}
	for _, v := range []struct{ id, action string }{{"exec-1", "execve"}, {"exec-2", "execve"}, {"stdin", "writeStdin"}} {
		r := f.review(v.id, v.action, true)
		target := "same-item"
		r.TargetItemID = &target
		f.recordReview(r)
	}
	if f.count("guardian_reviews") != 4 {
		t.Fatal("target incorrectly deduplicated reviews")
	}
	changed := f.review("network", "command", true)
	f.recordReview(changed)
	must(t, f.db.QueryRow("SELECT conflict FROM guardian_reviews WHERE review_id='network'").Scan(&conflict))
	if conflict != 1 {
		t.Fatal("immutable conflict not isolated")
	}
}
func TestA1A9ReviewFencesRetentionAndPurge(t *testing.T) {
	f := newFixture(t)
	f.chat()
	r := f.review("review", "execve", true)
	f.recordReview(r)
	if f.count("guardian_reviews") != 0 {
		t.Fatal("disabled recorded review")
	}
	f.enable(true)
	must(t, f.store.RecordGuardianReview(context.Background(), f.reviewScope("stale"), r, f.now))
	if f.count("guardian_reviews") != 0 {
		t.Fatal("stale controller recorded review")
	}
	f.recordReview(r)
	must(t, f.store.PurgeGuardian(context.Background(), f.session.ID, f.now))
	f.recordReview(r)
	if f.count("guardian_reviews") != 0 {
		t.Fatal("purged review revived")
	}
	fresh := f.review("fresh", "command", true)
	at := f.now.Add(time.Second)
	end := at.Add(time.Second)
	fresh.StartedAt = &at
	fresh.CompletedAt = &end
	f.now = end
	f.recordReview(fresh)
	if f.count("guardian_reviews") != 1 {
		t.Fatal("fresh review missing")
	}
	f.now = f.now.Add(domain.GuardianRetention + time.Second)
	must(t, f.store.CleanupGuardian(context.Background(), f.now))
	if f.count("guardian_reviews") != 0 {
		t.Fatal("review retained past deadline")
	}
	f.recordReview(fresh)
	if f.count("guardian_reviews") != 0 {
		t.Fatal("expired review revived")
	}
}

func (f *fixture) reviewScope(generation string) domain.GuardianReviewScope {
	return domain.GuardianReviewScope{SessionID: f.session.ID, Generation: generation, AccountID: domain.DeviceAgentAccountID(domain.AgentAccountProviderCodex), NativeRootID: "root", LaunchedAt: f.base}
}
func TestA1ReviewAccountScopeAndInvalidMetadata(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.chat()
	scope := f.reviewScope("generation")
	scope.AccountID = "other-account"
	must(t, f.store.RecordGuardianReview(context.Background(), scope, f.review("review", "command", true), f.now))
	if f.count("guardian_reviews") != 0 || f.codes()[domain.GuardianOwnerUnresolved] != 1 {
		t.Fatal("account scope mixed")
	}
	f.recordReview(domain.GuardianReview{InvalidMetadata: true})
	if f.codes()[domain.GuardianInvalidMetadata] != 1 {
		t.Fatal("invalid metadata silently dropped")
	}
}

func TestA9ReviewMissingStartCannotReviveAfterPurge(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.chat()
	r := f.review("review", "execve", true)
	f.recordReview(r)
	must(t, f.store.PurgeGuardian(context.Background(), f.session.ID, f.now))
	f.now = f.now.Add(time.Hour)
	r.StartedAt = nil
	done := f.now.Add(-time.Minute)
	r.CompletedAt = &done
	f.recordReview(r)
	if f.count("guardian_reviews") != 0 || f.codes()[domain.GuardianUnknownTime] != 1 {
		t.Fatal("missing start revived review")
	}
}

func TestA9ReviewRetainsMinimalThreadOwnerUntilExpiry(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.chat()
	f.write("guardian", f.meta("guardian", "root", true, f.base)+f.request("response", "turn", 100, 20))
	f.collect()
	f.now = f.base.Add(domain.GuardianRetention + time.Hour)
	r := f.review("nested-review", "execve", true)
	r.ParentThreadID = "guardian"
	start := f.now.Add(-2 * time.Minute)
	done := f.now.Add(-time.Minute)
	r.StartedAt = &start
	r.CompletedAt = &done
	f.recordReview(r)
	must(t, f.store.CleanupGuardian(context.Background(), f.now))
	if f.count("guardian_reviews") != 1 || f.count("guardian_requests") != 0 || f.count("guardian_threads") != 1 {
		t.Fatal("review lost minimal owner or retained old request")
	}
	var version string
	must(t, f.db.QueryRow("SELECT cli_version FROM guardian_threads").Scan(&version))
	if version != "" {
		t.Fatal("expired thread retained version metadata")
	}
	f.now = f.now.Add(domain.GuardianRetention + time.Hour)
	must(t, f.store.CleanupGuardian(context.Background(), f.now))
	if f.count("guardian_reviews") != 0 || f.count("guardian_threads") != 0 {
		t.Fatal("owner survived its last review reference")
	}
}
