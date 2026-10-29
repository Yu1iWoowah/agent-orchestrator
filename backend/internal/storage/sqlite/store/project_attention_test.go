package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReportForwardingIsAtomicScopedAndSurvivesOrchestratorReplacement(t *testing.T) {
	s, worker, _ := conversationFixture(t)
	ctx := context.Background()
	orchestrator, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "hist", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: histClock}, CreatedAt: histClock, UpdatedAt: histClock})
	if err != nil {
		t.Fatal(err)
	}
	report := domain.ReportRecord{ID: "report-1", SessionID: worker, ProjectID: "hist", State: domain.ReportNeedsInput, Note: "Need a human decision", CreatedAt: histClock, AvailableAt: histClock, RepeatCount: 1, DeliveryState: domain.ReportPending}
	if _, err = s.CreateReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	command := askCreate(orchestrator.ID)
	command.SourceReportID = report.ID
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
	replacement, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "hist", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: histClock}, CreatedAt: histClock.Add(time.Second), UpdatedAt: histClock})
	if err != nil {
		t.Fatal(err)
	}
	command.OriginSessionID = orchestrator.ID
	if _, _, err = s.CreateAttentionQuestion(ctx, command, histClock); !errors.Is(err, domain.ErrAttentionQuestionForbidden) {
		t.Fatalf("superseded coordinator: %v", err)
	}
	command.OriginSessionID = replacement.ID
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
