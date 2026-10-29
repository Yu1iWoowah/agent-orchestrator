package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func TestReportForwardingIsAtomicScopedAndSurvivesOrchestratorReplacement(t *testing.T) {
	s, worker, _ := conversationFixture(t)
	ctx := context.Background()
	orchestrator, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "hist", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{BrowserCapabilityVerifier: "first-proof"}, Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: histClock}, CreatedAt: histClock, UpdatedAt: histClock})
	if err != nil {
		t.Fatal(err)
	}
	report := domain.ReportRecord{ID: "report-1", SessionID: worker, ProjectID: "hist", State: domain.ReportNeedsInput, Note: "Need a human decision", CreatedAt: histClock, AvailableAt: histClock, RepeatCount: 1, DeliveryState: domain.ReportPending}
	if _, err = s.CreateReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	command := askCreate(orchestrator.ID)
	command.SourceReportID = report.ID
	command.ActorVerifier = orchestrator.Metadata.BrowserCapabilityVerifier
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, _, err := s.CreateAttentionQuestion(ctx, command, histClock)
			if err != nil || q.OriginSessionID != worker || q.SourceReportID != report.ID {
				t.Errorf("forward: %+v %v", q, err)
			}
		}()
	}
	wg.Wait()
	page, err := s.ListAttentionQuestions(ctx, worker, domain.AttentionQuestionQuery{}, histClock)
	if err != nil || page.Open != 1 {
		t.Fatalf("canonical questions: %+v %v", page, err)
	}
	saved, found, err := s.GetReport(ctx, report.ID)
	if err != nil || !found || saved.DeliveryState != domain.ReportPending {
		t.Fatalf("forward changed U1 delivery: %+v %v", saved, err)
	}
	// Caller identity is looked up, never accepted from a role string or source claim.
	command.OriginSessionID = worker
	if _, _, err = s.CreateAttentionQuestion(ctx, command, histClock); !errors.Is(err, domain.ErrAttentionQuestionForbidden) {
		t.Fatalf("worker impersonation: %v", err)
	}
	replacement, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "hist", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{BrowserCapabilityVerifier: "replacement-proof"}, Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: histClock}, CreatedAt: histClock.Add(time.Second), UpdatedAt: histClock})
	if err != nil {
		t.Fatal(err)
	}
	command.OriginSessionID = orchestrator.ID
	if _, _, err = s.CreateAttentionQuestion(ctx, command, histClock); !errors.Is(err, domain.ErrAttentionQuestionForbidden) {
		t.Fatalf("superseded coordinator: %v", err)
	}
	command.OriginSessionID = replacement.ID
	if _, _, err = s.CreateAttentionQuestion(ctx, command, histClock); !errors.Is(err, domain.ErrAttentionQuestionForbidden) {
		t.Fatalf("stale authenticated verifier accepted: %v", err)
	}
	command.ActorVerifier = replacement.Metadata.BrowserCapabilityVerifier
	q, created, err := s.CreateAttentionQuestion(ctx, command, histClock)
	if err != nil || created || q.ID != page.Items[0].ID {
		t.Fatalf("replacement duplicated or lost question: %+v %v %v", q, created, err)
	}
}

func TestProjectAttentionSeparatesAudiencesCountsAndPages(t *testing.T) {
	s, worker, _ := conversationFixture(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		cmd := askCreate(worker)
		cmd.ID = fmt.Sprintf("question-%d", i)
		cmd.ClientKey = cmd.ID
		if _, _, err := s.CreateAttentionQuestion(ctx, cmd, histClock); err != nil {
			t.Fatal(err)
		}
	}
	report := domain.ReportRecord{ID: "report-1", SessionID: worker, ProjectID: "hist", State: domain.ReportNeedsInput, Note: "Coordinate first", CreatedAt: histClock, AvailableAt: histClock, RepeatCount: 1, DeliveryState: domain.ReportPending}
	if _, err := s.CreateReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	query := domain.ProjectAttentionQuery{Audience: "user", Limit: 2}
	first, err := s.ProjectAttention(ctx, "hist", query, histClock)
	if err != nil || first.Open != 3 || first.Unread != 3 || len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first: %+v %v", first, err)
	}
	query.Cursor = first.NextCursor
	last, err := s.ProjectAttention(ctx, "hist", query, histClock)
	if err != nil || last.Open != 3 || len(last.Items) != 1 || last.NextCursor != "" || last.Items[0].ID == first.Items[0].ID {
		t.Fatalf("last: %+v %v", last, err)
	}
	reports, err := s.ProjectAttention(ctx, "hist", domain.ProjectAttentionQuery{Audience: "orchestrator"}, histClock)
	if err != nil || reports.Open != 1 || reports.Items[0].AnswerState != "unknown" || reports.Items[0].ReportID != report.ID {
		t.Fatalf("reports: %+v %v", reports, err)
	}
	if err := s.ReadProjectAttentionReport(ctx, "hist", report.ID, histClock); err != nil {
		t.Fatal(err)
	}
	reports, err = s.ProjectAttention(ctx, "hist", domain.ProjectAttentionQuery{Audience: "orchestrator"}, histClock)
	if err != nil || reports.Unread != 0 || reports.Open != 1 {
		t.Fatalf("reading must not close: %+v %v", reports, err)
	}
	if err := s.ReadProjectAttentionReport(ctx, "other", report.ID, histClock); !errors.Is(err, domain.ErrAttentionQuestionNotFound) {
		t.Fatalf("cross-project read: %v", err)
	}
	if _, err = s.ProjectAttention(ctx, "hist", domain.ProjectAttentionQuery{Audience: "invented"}, histClock); err == nil {
		t.Fatal("unknown audience accepted")
	}
}

func TestAttentionNotificationRevisionSurvivesReadClearAndReconnect(t *testing.T) {
	s, worker, _ := conversationFixture(t)
	ctx := context.Background()
	command := askCreate(worker)
	question, _, err := s.CreateAttentionQuestion(ctx, command, histClock)
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("attention:question:%s:1", question.ID)
	rec := domain.NotificationRecord{ID: "notice-1", SessionID: worker, ProjectID: "hist", Type: domain.NotificationNeedsInput, Title: "Decision", Status: domain.NotificationUnread, CreatedAt: histClock, SourceKey: key}
	if _, inserted, err := s.CreateNotification(ctx, rec); err != nil || !inserted {
		t.Fatalf("first: %v %v", inserted, err)
	}
	if _, _, err := s.MarkNotificationRead(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	// Session activity may remain active while a durable question is open.
	if _, err := s.ReconcileResolvedNotifications(ctx, histClock); err != nil {
		t.Fatal(err)
	}
	pending, err := s.CountUnresolvedNotifications(ctx)
	if err != nil || pending != 1 {
		t.Fatalf("activity incorrectly resolved question: %d %v", pending, err)
	}
	if _, err := s.ClearAllNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelAttentionQuestion(ctx, worker, question.ID, 1, "user", "No longer needed", histClock); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileAttentionNotifications(ctx, "hist", nil, histClock); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearAllNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	rec.ID = "reconnected"
	if _, inserted, err := s.CreateNotification(ctx, rec); err != nil || inserted {
		t.Fatalf("reconnect replayed cleared source: %v %v", inserted, err)
	}
}

func createAttentionReport(t *testing.T, s *sqlite.Store, worker domain.SessionID, id string, state domain.ReportState, at time.Time) {
	t.Helper()
	record := domain.ReportRecord{ID: id, SessionID: worker, ProjectID: "hist", State: state, Note: id, CreatedAt: at, AvailableAt: at, RepeatCount: 1, DeliveryState: domain.ReportPending}
	if state == domain.ReportDone {
		record.SettlementDeadline = at.Add(domain.ReportSettlementWindow)
		record.AvailableAt = record.SettlementDeadline
	}
	if _, err := s.CreateReport(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

// Reproduces audit65 I3: unrelated progress is neither an answer nor a handoff.
func TestUnresolvedReportSurvivesUnrelatedCheckpoint(t *testing.T) {
	s, worker, _ := conversationFixture(t)
	ctx := context.Background()
	createAttentionReport(t, s, worker, "unresolved-decision", domain.ReportNeedsInput, histClock)
	createAttentionReport(t, s, worker, "unrelated-checkpoint", domain.ReportCheckpoint, histClock.Add(time.Second))
	saved, found, err := s.GetReport(ctx, "unresolved-decision")
	if err != nil || !found || saved.State != domain.ReportNeedsInput || saved.DeliveryState != domain.ReportPending {
		t.Fatalf("fixture lost pending report: %+v %v", saved, err)
	}
	questions, err := s.ListAttentionQuestions(ctx, worker, domain.AttentionQuestionQuery{}, histClock.Add(2*time.Second))
	if err != nil || questions.Open != 0 {
		t.Fatalf("unexpected canonical handoff: %+v %v", questions, err)
	}
	page, err := s.ProjectAttention(ctx, "hist", domain.ProjectAttentionQuery{Audience: "orchestrator"}, histClock.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if page.Open != 1 || len(page.Items) != 1 || page.Items[0].ReportID != saved.ID {
		t.Fatalf("pending unanswered report silently disappeared after unrelated checkpoint: open=%d items=%d; source state=%s delivery=%s canonicalQuestions=%d", page.Open, len(page.Items), saved.State, saved.DeliveryState, questions.Open)
	}
}

func TestProjectReportsExcludeTerminatedWorkersAndSupersededReports(t *testing.T) {
	for _, tc := range []struct {
		name       string
		newer      []domain.ReportState
		terminated bool
		want       int64
	}{
		{"multiple needs_input", []domain.ReportState{domain.ReportNeedsInput, domain.ReportNeedsInput}, false, 1},
		{"stuck replaces needs_input", []domain.ReportState{domain.ReportStuck}, false, 1},
		{"done closes needs_input", []domain.ReportState{domain.ReportDone}, false, 0},
		{"worker ended", nil, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, worker, _ := conversationFixture(t)
			ctx := context.Background()
			createAttentionReport(t, s, worker, "first", domain.ReportNeedsInput, histClock)
			var latest string
			for i, state := range tc.newer {
				latest = fmt.Sprintf("newer-%d", i)
				createAttentionReport(t, s, worker, latest, state, histClock.Add(time.Duration(i+1)*time.Second))
			}
			if tc.terminated {
				record, found, err := s.GetSession(ctx, worker)
				if err != nil || !found {
					t.Fatalf("worker: %v %v", found, err)
				}
				record.IsTerminated = true
				record.UpdatedAt = histClock.Add(time.Minute)
				if err := s.UpdateSession(ctx, record); err != nil {
					t.Fatal(err)
				}
			}
			page, err := s.ProjectAttention(ctx, "hist", domain.ProjectAttentionQuery{Audience: "orchestrator"}, histClock.Add(time.Minute))
			if err != nil || page.Open != tc.want || int64(len(page.Items)) != tc.want {
				t.Fatalf("open reports: %+v %v", page, err)
			}
			if tc.want == 1 && (page.Items[0].ReportID != latest || page.Items[0].AnswerState != "unknown") {
				t.Fatalf("wrong replacement: %+v", page)
			}
			report, found, err := s.GetReport(ctx, "first")
			if err != nil || !found || report.State != domain.ReportNeedsInput || report.DeliveryState != domain.ReportPending {
				t.Fatalf("indexing rewrote source delivery or state: %+v %v", report, err)
			}
		})
	}
}

func TestForwardedReportFollowsCanonicalQuestionLifecycle(t *testing.T) {
	for _, closed := range []string{"answered", "cancelled", "expired"} {
		t.Run(closed, func(t *testing.T) {
			s, worker, _ := conversationFixture(t)
			ctx := context.Background()
			createAttentionReport(t, s, worker, "forwarded", domain.ReportNeedsInput, histClock)
			orchestrator, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "hist", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{BrowserCapabilityVerifier: "verified-actor"}, CreatedAt: histClock, UpdatedAt: histClock})
			if err != nil {
				t.Fatal(err)
			}
			deadline := histClock.Add(time.Minute)
			input := askCreate(orchestrator.ID)
			input.SourceReportID = "forwarded"
			input.ActorVerifier = orchestrator.Metadata.BrowserCapabilityVerifier
			input.ExpiresAt = &deadline
			question, _, err := s.CreateAttentionQuestion(ctx, input, histClock)
			if err != nil {
				t.Fatal(err)
			}
			assertReports := func(at time.Time, ids ...string) {
				t.Helper()
				page, err := s.ProjectAttention(ctx, "hist", domain.ProjectAttentionQuery{Audience: "orchestrator"}, at)
				if err != nil || page.Open != int64(len(ids)) || len(page.Items) != len(ids) {
					t.Fatalf("canonical report lifecycle: %+v %v, want=%v", page, err, ids)
				}
				have := map[string]bool{}
				for _, item := range page.Items {
					have[item.ReportID] = true
					if item.ReportID == "forwarded" && item.QuestionID != question.ID {
						t.Fatalf("canonical link lost: %+v", item)
					}
				}
				for _, id := range ids {
					if !have[id] {
						t.Fatalf("report %q absent: %+v", id, page)
					}
				}
			}
			assertReports(histClock, "forwarded")
			createAttentionReport(t, s, worker, "another-decision", domain.ReportNeedsInput, histClock.Add(time.Second))
			assertReports(histClock.Add(time.Second), "forwarded", "another-decision")
			createAttentionReport(t, s, worker, "checkpoint", domain.ReportCheckpoint, histClock.Add(2*time.Second))
			assertReports(histClock.Add(2*time.Second), "forwarded", "another-decision")
			createAttentionReport(t, s, worker, "done", domain.ReportDone, histClock.Add(3*time.Second))
			assertReports(histClock.Add(3*time.Second), "forwarded")
			at := histClock.Add(4 * time.Second)
			switch closed {
			case "answered":
				_, err = s.AnswerAttentionQuestion(ctx, worker, askAnswer(question.ID), at)
			case "cancelled":
				_, err = s.CancelAttentionQuestion(ctx, worker, question.ID, 1, "user", "No longer needed", at)
			case "expired":
				at = deadline.Add(time.Second)
				assertReports(at)
				err = s.ExpireAttentionQuestions(ctx, worker, at)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertReports(at)
			current, err := s.GetAttentionQuestion(ctx, worker, question.ID)
			if err != nil || current.State != closed {
				t.Fatalf("question lifecycle: %+v %v", current, err)
			}
			report, found, err := s.GetReport(ctx, "forwarded")
			if err != nil || !found || report.State != domain.ReportNeedsInput || report.DeliveryState != domain.ReportPending {
				t.Fatalf("question state must not rewrite U1 report: %+v %v", report, err)
			}
		})
	}
}
