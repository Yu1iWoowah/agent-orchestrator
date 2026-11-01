package guardian

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestGuardianReviewPendingArchiveRecovery(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.chat()
	ctx := context.Background()
	conv, err := f.store.CreateConversation(ctx, "conversation", domain.ConversationScopeSession, "project", f.session.ID, f.now)
	must(t, err)
	// CreateConversation can rebind the owner; establish the live capture fence.
	f.chat()
	r := f.review("completed-only", "networkAccess", true)
	capture := func(tx context.Context) error {
		if _, err := f.store.QueueGuardianReview(tx, f.reviewScope("generation"), r, f.now); err != nil {
			return err
		}
		return f.store.UpsertActivity(tx, conv.ID, "turn", domain.ConversationActivity{ID: "activity", ProviderItemID: "auto-review", Kind: domain.ActivityKindAutoReview, Status: domain.ActivityStatusCompleted}, f.now)
	}
	// A failed projection rolls both its archive and pending metadata back.
	projected, err := f.store.ProjectProviderEvent(ctx, conv.ID, f.session.ID, "generation", "event", "review", "{}", f.now, func(tx context.Context) error {
		if err := capture(tx); err != nil {
			return err
		}
		return errors.New("synthetic rollback")
	})
	if err == nil || projected || f.count("guardian_review_pending") != 0 || f.count("conversation_provider_events") != 0 || f.count("conversation_activities") != 0 {
		t.Fatalf("partial commit: projected=%v err=%v", projected, err)
	}
	projected, err = f.store.ProjectProviderEvent(ctx, conv.ID, f.session.ID, "generation", "event", "review", "{}", f.now, capture)
	must(t, err)
	if !projected || f.count("guardian_review_pending") != 1 || f.count("guardian_reviews") != 0 || f.count("conversation_activities") != 1 {
		t.Fatal("archive did not durably capture pending metadata")
	}
	firstSeen := f.now
	// A real apply failure cannot undo committed activity or its durable pending.
	_, err = f.db.Exec(`CREATE TRIGGER fail_guardian_apply BEFORE INSERT ON guardian_reviews BEGIN SELECT RAISE(FAIL,'private apply error sentinel'); END;`)
	must(t, err)
	err = f.store.RecordGuardianReview(ctx, f.reviewScope("generation"), r, f.now)
	if err == nil || f.count("guardian_review_pending") != 1 || f.count("guardian_reviews") != 0 || f.count("conversation_activities") != 1 {
		t.Fatal("temporary apply failure lost durable pending or activity")
	}
	_, err = f.db.Exec("DROP TRIGGER fail_guardian_apply")
	must(t, err)
	// Recovery does not depend on the old controller generation or live owner.
	_, err = f.db.Exec("UPDATE sessions SET controller_generation='replacement',is_terminated=1 WHERE id=?", f.session.ID)
	must(t, err)
	reopened, err := sqlite.OpenPreMigrated(filepath.Dir(f.root))
	must(t, err)
	defer func() { must(t, reopened.Close()) }()
	recovery := New(reopened, Config{Clock: func() time.Time { return f.now.Add(time.Hour) }})
	must(t, recovery.Collect(ctx))
	must(t, recovery.Collect(ctx))
	if f.count("guardian_review_pending") != 0 || f.count("guardian_reviews") != 1 || f.count("conversation_activities") != 1 {
		t.Fatal("recovery lost or duplicated facts")
	}
	var first, retention time.Time
	var completed, started int
	must(t, f.db.QueryRow("SELECT first_seen_at,retention_at,saw_started,saw_completed FROM guardian_reviews").Scan(&first, &retention, &started, &completed))
	if !first.Equal(firstSeen) || !retention.Equal(*r.StartedAt) || started != 0 || completed != 1 {
		t.Fatal("recovery refreshed time or changed completed-only lifecycle")
	}
}

func TestGuardianPendingPurgeExpiryAndOptIn(t *testing.T) {
	for _, mode := range []string{"purge", "expiry", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.enable(true)
			f.chat()
			ctx := context.Background()
			accepted, err := f.store.QueueGuardianReview(ctx, f.reviewScope("generation"), f.review("pending", "execve", true), f.now)
			must(t, err)
			if !accepted {
				t.Fatal("valid pending rejected")
			}
			if f.count("guardian_review_pending") != 1 {
				t.Fatal("pending not queued")
			}
			switch mode {
			case "purge":
				must(t, f.store.PurgeGuardian(ctx, f.session.ID, f.now.Add(time.Minute)))
			case "expiry":
				f.now = f.now.Add(domain.GuardianRetention + time.Hour)
				must(t, f.store.CleanupGuardian(ctx, f.now))
			case "disabled":
				f.enable(false)
			}
			must(t, f.store.ReplayGuardianReviews(ctx, f.now))
			if f.count("guardian_reviews") != 0 {
				t.Fatal("purged/expired/disabled pending was applied")
			}
			if mode != "disabled" && f.count("guardian_review_pending") != 0 {
				t.Fatal("pending survived purge/expiry")
			}
			if mode == "disabled" {
				f.enable(true)
				must(t, f.store.ReplayGuardianReviews(ctx, f.now))
				if f.count("guardian_reviews") != 1 {
					t.Fatal("validated pending was lost while disabled")
				}
			}
		})
	}
}

func TestGuardianCaptureFailureRollsBackOnlyDiagnostics(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.chat()
	ctx := context.Background()
	conv, err := f.store.CreateConversation(ctx, "conversation", domain.ConversationScopeSession, "project", f.session.ID, f.now)
	must(t, err)
	f.chat()
	_, err = f.db.Exec(`CREATE TRIGGER fail_guardian_capture AFTER INSERT ON guardian_review_pending BEGIN SELECT RAISE(FAIL,'private capture error sentinel'); END;`)
	must(t, err)
	captureFailures := 0
	projected, err := f.store.ProjectProviderEvent(ctx, conv.ID, f.session.ID, "generation", "event", "review", "{}", f.now, func(tx context.Context) error {
		accepted, captureErr := f.store.QueueGuardianReview(tx, f.reviewScope("generation"), f.review("review", "execve", true), f.now)
		if captureErr == nil || accepted {
			t.Error("fault injection did not reject capture")
		}
		captureFailures++
		return f.store.UpsertActivity(tx, conv.ID, "turn", domain.ConversationActivity{ID: "activity", ProviderItemID: "auto-review", Kind: domain.ActivityKindAutoReview, Status: domain.ActivityStatusCompleted}, f.now)
	})
	must(t, err)
	if !projected || captureFailures != 1 || f.count("conversation_activities") != 1 || f.count("conversation_provider_events") != 1 {
		t.Fatal("diagnostic failure lost the normal projection")
	}
	if f.count("guardian_review_pending") != 0 || f.count("guardian_epochs") != 0 {
		t.Fatal("failed capture leaked partial diagnostic writes")
	}
	_, err = f.db.Exec("DROP TRIGGER fail_guardian_capture")
	must(t, err)
	// Once storage recovers, the same valid observation can still be captured.
	accepted, err := f.store.QueueGuardianReview(ctx, f.reviewScope("generation"), f.review("review", "execve", true), f.now)
	must(t, err)
	if !accepted {
		t.Fatal("capture did not recover")
	}
	must(t, f.store.ReplayGuardianReviews(ctx, f.now))
	if f.count("guardian_reviews") != 1 {
		t.Fatal("recovered observation missing")
	}
}
