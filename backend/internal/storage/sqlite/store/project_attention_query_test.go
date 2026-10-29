package store_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	sqlitedriver "modernc.org/sqlite"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Count real SQLite queries, including those inside the read-only snapshot.
type attentionCountingDriver struct {
	driver.Driver
	queries *atomic.Int64
}

func (d attentionCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return attentionCountingConn{Conn: conn, queries: d.queries}, nil
}

type attentionCountingConn struct {
	driver.Conn
	queries *atomic.Int64
}

func (c attentionCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.queries.Add(1)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c attentionCountingConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
}

func TestProjectAttentionQueryCountIgnoresTerminatedSessions(t *testing.T) {
	directory := t.TempDir()
	s := sqlitetest.MustOpenAt(t, directory)
	ctx := context.Background()
	seedProject(t, s, "hist")
	record := sampleRecord("hist")
	record.Mode = domain.SessionModeChat
	active, err := s.CreateSession(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := s.CreateConversation(ctx, "active", domain.ConversationScopeSession, "hist", active.ID, histClock)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimChatControllerGeneration(ctx, active.ID, "gen-1"); err != nil {
		t.Fatal(err)
	}
	appendPlanTurn(t, s, conversation.ID, active.ID, "pending", histClock)
	attentionActivity(t, s, conversation.ID, "pending", "active-request", domain.ActivityKindApproval, domain.ActivityStatusPending)
	if _, _, err := s.CreateAttentionQuestion(ctx, askCreate(active.ID), histClock); err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	name := "attention-count-" + uuid.NewString()
	sql.Register(name, attentionCountingDriver{Driver: &sqlitedriver.Driver{}, queries: &queries})
	reader, err := sql.Open(name, "file:"+filepath.Join(directory, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	observed := store.NewStore(reader, reader)
	measure := func() int64 {
		t.Helper()
		queries.Store(0)
		page, err := observed.ProjectAttention(ctx, "hist", domain.ProjectAttentionQuery{Audience: "user"}, histClock)
		if err != nil || page.Open != 2 || len(page.Items) != 2 {
			t.Fatalf("active question and provider source changed: %+v %v", page, err)
		}
		return queries.Load()
	}
	before := measure()
	for i := range 250 {
		record := sampleRecord("hist")
		record.IsTerminated = true
		ended, err := s.CreateSession(ctx, record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateConversation(ctx, fmt.Sprintf("ended-%d", i), domain.ConversationScopeSession, "hist", ended.ID, histClock); err != nil {
			t.Fatal(err)
		}
	}
	after := measure()
	t.Logf("attention queries with 0/250 ended sessions: %d/%d", before, after)
	if after != before {
		t.Fatalf("250 ended sessions increased attention queries: before=%d after=%d", before, after)
	}
}

func TestAttentionNotificationSourcesDoNotTruncateAtTheUIPageLimit(t *testing.T) {
	s, session, _ := conversationFixture(t)
	ctx := context.Background()
	for i := range 130 {
		command := askCreate(session)
		command.ID = fmt.Sprintf("question-%d", i)
		command.ClientKey = command.ID
		if _, _, err := s.CreateAttentionQuestion(ctx, command, histClock); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.AttentionNotificationSources(ctx, "hist", histClock)
	if err != nil || len(items) != 130 {
		t.Fatalf("notification snapshot truncated: %d %v", len(items), err)
	}
	page, err := s.ProjectAttention(ctx, "hist", domain.ProjectAttentionQuery{Limit: 100}, histClock)
	if err != nil || len(page.Items) != 100 || page.Open != 130 || page.NextCursor == "" {
		t.Fatalf("UI pagination changed: %+v %v", page, err)
	}
}

func TestAttentionSourceRevisionsIncludeUnchangedActiveProjectsOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "active")
	seedProject(t, s, "archived")
	if archived, err := s.ArchiveProject(ctx, "archived", histClock); err != nil || !archived {
		t.Fatalf("archive fixture: %v %v", archived, err)
	}
	revisions, err := s.AttentionSourceRevisions(ctx)
	if err != nil || len(revisions) != 1 {
		t.Fatalf("active project generations: %v %v", revisions, err)
	}
	if revision, found := revisions["active"]; !found || revision != 0 {
		t.Fatalf("unversioned project must still bootstrap once: %v", revisions)
	}
}
