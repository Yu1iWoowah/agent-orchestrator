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

// BenchmarkInputCustodyOrdinaryWrites measures v1 writes and fresh reader setup
// with no registered custody. Fixture migration is outside the timed region.
// Run with a fixed benchtime and compare alternating builds of main and S0.
func BenchmarkInputCustodyOrdinaryWrites(b *testing.B) {
	for _, operation := range []string{"session-update", "append-input", "fresh-store"} {
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
			b.ReportMetric(float64(b.N), "completed-ops")
		})
	}
}
