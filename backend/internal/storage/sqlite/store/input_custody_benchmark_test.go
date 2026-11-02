package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// BenchmarkInputCustodyOrdinaryWrites measures v1 writes and fresh store setup
// with no registered custody. Fixture migration is outside the timed region.
// Run with a fixed benchtime and compare alternating builds of main and S0.
func BenchmarkInputCustodyOrdinaryWrites(b *testing.B) {
	for _, operation := range []string{"session-update", "append-input", "message-update", "activity-insert", "activity-update", "fresh-store"} {
		b.Run(operation, func(b *testing.B) {
			ctx := context.Background()
			dir := b.TempDir()
			s, err := sqlitetest.Open(dir)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "bench", Path: dir, RegisteredAt: now}); err != nil {
				b.Fatal(err)
			}
			rec, err := s.CreateSession(ctx, domain.SessionRecord{ProjectID: "bench", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now}, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := s.CreateConversation(ctx, "bench", domain.ConversationScopeSession, "bench", rec.ID, now); err != nil {
				b.Fatal(err)
			}
			if operation == "message-update" || operation == "activity-insert" || operation == "activity-update" {
				if _, err := s.AppendUserMessage(ctx, "bench", rec.ID, "generation", domain.ConversationMessage{ID: "seed-message", Text: "seed input", Origin: domain.MessageOriginHuman}, "seed-turn", now); err != nil {
					b.Fatal(err)
				}
			}
			if operation == "activity-insert" || operation == "activity-update" {
				if err := s.BindTurnToProvider(ctx, "seed-turn", "provider-turn", now); err != nil {
					b.Fatal(err)
				}
			}
			if operation == "activity-update" {
				if err := s.UpsertActivity(ctx, "bench", "provider-turn", domain.ConversationActivity{ID: "activity", ProviderItemID: "item", Kind: domain.ActivityKindCommand, Status: domain.ActivityStatusRunning, Summary: "seed"}, now); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				switch operation {
				case "session-update":
					rec.UpdatedAt = now.Add(time.Duration(i+1) * time.Second)
					rec.Activity.LastActivityAt = rec.UpdatedAt
					if err := s.UpdateSession(ctx, rec); err != nil {
						b.Fatal(err)
					}
				case "append-input":
					_, err := s.AppendUserMessage(ctx, "bench", rec.ID, "generation", domain.ConversationMessage{ID: fmt.Sprintf("message-%d", i), Text: "ordinary input", Origin: domain.MessageOriginHuman}, fmt.Sprintf("turn-%d", i), now.Add(time.Duration(i)*time.Second))
					if err != nil {
						b.Fatal(err)
					}
				case "message-update":
					if err := s.UpdateQueuedTurnMessage(ctx, "bench", "seed-turn", fmt.Sprintf("updated-%d", i), "", int64(i), now.Add(time.Duration(i+1)*time.Second), domain.ConversationQueuedEditDelivery{}); err != nil {
						b.Fatal(err)
					}
				case "activity-insert", "activity-update":
					activityID, providerItemID := "activity", "item"
					if operation == "activity-insert" {
						activityID, providerItemID = fmt.Sprintf("activity-%d", i), fmt.Sprintf("item-%d", i)
					}
					if err := s.UpsertActivity(ctx, "bench", "provider-turn", domain.ConversationActivity{ID: activityID, ProviderItemID: providerItemID, Kind: domain.ActivityKindCommand, Status: domain.ActivityStatusRunning, Summary: fmt.Sprintf("updated-%d", i)}, now.Add(time.Duration(i+1)*time.Second)); err != nil {
						b.Fatal(err)
					}
				case "fresh-store":
					opened, err := sqlite.OpenPreMigrated(dir)
					if err != nil {
						b.Fatal(err)
					}
					_, found, readErr := opened.GetSession(ctx, rec.ID)
					closeErr := opened.Close()
					if readErr != nil || !found || closeErr != nil {
						b.Fatalf("fresh read: found=%v read=%v close=%v", found, readErr, closeErr)
					}
				}
			}
			b.StopTimer()
			got, found, err := s.GetSession(ctx, rec.ID)
			if err != nil || !found {
				b.Fatalf("session readback: found=%v err=%v", found, err)
			}
			if operation == "session-update" && !got.UpdatedAt.Equal(rec.UpdatedAt) {
				b.Fatal("session writes did not persist")
			}
			if operation == "append-input" {
				for i := 0; i < b.N; i++ {
					turnID := fmt.Sprintf("turn-%d", i)
					if _, err := s.TurnByID(ctx, turnID); err != nil {
						b.Fatal(err)
					}
					message, err := s.QueuedTurnMessage(ctx, "bench", turnID)
					if err != nil || message.Text != "ordinary input" {
						b.Fatalf("append readback: %+v %v", message, err)
					}
				}
			}
			if operation == "message-update" {
				message, err := s.QueuedTurnMessage(ctx, "bench", "seed-turn")
				if err != nil || message.Text != fmt.Sprintf("updated-%d", b.N-1) || message.Revision != int64(b.N) {
					b.Fatalf("message update readback: %+v %v", message, err)
				}
			}
			if operation == "activity-insert" || operation == "activity-update" {
				snapshot, err := s.LoadConversationSnapshot(ctx, "bench")
				want := 1
				if operation == "activity-insert" {
					want = b.N
				}
				if err != nil || len(snapshot.Activities) != want {
					b.Fatalf("activity readback: count=%d want=%d err=%v", len(snapshot.Activities), want, err)
				}
				for i, activity := range snapshot.Activities {
					index := i
					var revision int64
					if operation == "activity-update" {
						index = b.N - 1
						revision = int64(b.N)
					}
					if activity.Summary != fmt.Sprintf("updated-%d", index) || activity.Status != domain.ActivityStatusRunning || activity.TurnID != "seed-turn" || activity.Revision != revision {
						b.Fatalf("activity write did not persist: %+v", activity)
					}
				}
			}
			b.ReportMetric(float64(b.N), "completed-ops")
		})
	}
}
