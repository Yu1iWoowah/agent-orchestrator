package chat

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// countingBranchStore is the real store, counting full snapshot loads and
// point queries so the test can see which one branchUnanswered used.
type countingBranchStore struct {
	*sqlite.Store
	snapshots, points int
}

func (c *countingBranchStore) LoadConversationSnapshot(ctx context.Context, id string) (sqlitestore.ConversationSnapshot, error) {
	c.snapshots++
	return c.Store.LoadConversationSnapshot(ctx, id)
}

func (c *countingBranchStore) ConversationBranchHasVisibleTurn(ctx context.Context, conversationID, branchID string) (bool, error) {
	c.points++
	return c.Store.ConversationBranchHasVisibleTurn(ctx, conversationID, branchID)
}

// branchUnanswered answers from one point query, with the same result the
// full snapshot used to give for every branch a resume can ask about.
func TestBranchUnansweredUsesPointQueryWithSnapshotSemantics(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dir)
	const session = domain.SessionID("p1-1")
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	// One project conversation each: lineage, and empty (no turn on its active branch).
	conversation := func(project domain.ProjectID, owner domain.SessionID, id string) domain.ConversationRecord {
		t.Helper()
		if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: string(project), Path: filepath.Join(dir, string(project)), RegisteredAt: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateSession(ctx, domain.SessionRecord{ID: owner, ProjectID: project, Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		record, err := st.CreateConversation(ctx, id, domain.ConversationScopeProject, project, owner, now)
		if err != nil || record.ID != id {
			t.Fatalf("CreateConversation %s = %s, %v", id, record.ID, err)
		}
		return record
	}
	lineage := conversation("p1", session, "lineage")
	empty := conversation("p2", "p2-1", "empty")
	root, emptyRoot := lineage.ActiveBranchID, empty.ActiveBranchID

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ao.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	branch := func(conversation, id, parent string, forkAfter int) {
		exec(`INSERT INTO conversation_branches(id,conversation_id,session_id,parent_branch_id,fork_after_sequence,created_at) VALUES(?,?,?,?,?,?)`,
			id, conversation, session, parent, forkAfter, now)
	}
	// turn records one turn whose first message sits at sequence on branch.
	turn := func(conversation, id, branch string, sequence int, state string, promotedTo any, rolledBack any) {
		exec(`INSERT INTO conversation_turns(id,conversation_id,handled_by_session_id,state,requested_at,branch_id,promoted_to_turn_id,rolled_back_at) VALUES(?,?,?,?,?,?,?,?)`,
			id, conversation, session, state, now.Add(time.Duration(sequence)*time.Second), branch, promotedTo, rolledBack)
		exec(`INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,created_at,updated_at,branch_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			"message-"+id, conversation, id, sequence, "user", "human", "prompt "+id, now, now, branch)
	}

	// Active lineage root -> b7 -> b8 -> b3 -> mid -> act; side forks off root.
	// Each ancestor is visible only up to the earliest fork below it.
	turn("lineage", "root-turn", root, 1, "completed", nil, nil)
	branch("lineage", "b7", root, 1)
	turn("lineage", "rolled-back", "b7", 2, "completed", nil, now)
	branch("lineage", "b8", "b7", 2)
	turn("lineage", "running", "b8", 3, "running", nil, nil)
	branch("lineage", "b3", "b8", 3)
	turn("lineage", "promoted", "b3", 4, "completed", "root-turn", nil)
	branch("lineage", "mid", "b3", 4)
	branch("lineage", "act", "mid", 5)
	turn("lineage", "after-fork", "mid", 6, "completed", nil, nil)
	turn("lineage", "active-turn", "act", 7, "completed", nil, nil)
	branch("lineage", "side", root, 1)
	turn("lineage", "side-turn", "side", 8, "completed", nil, nil)
	exec(`UPDATE conversations SET active_branch_id='act', latest_sequence=8 WHERE id='lineage'`)
	// A second conversation whose active branch has no turn yet.
	turn("empty", "empty-root-turn", emptyRoot, 1, "completed", nil, nil)
	branch("empty", "fresh", emptyRoot, 1)
	exec(`UPDATE conversations SET active_branch_id='fresh', latest_sequence=1 WHERE id='empty'`)

	own := func(id string) domain.ConversationBranch {
		return domain.ConversationBranch{ID: id, ParentBranchID: "parent", SessionID: session}
	}
	for _, tc := range []struct {
		name, conversation string
		branch             domain.ConversationBranch
		want, queries      bool
	}{
		{"active branch with a turn", "lineage", own("act"), false, true},
		{"active branch without a turn", "empty", own("fresh"), true, true},
		{"only a promoted turn", "lineage", own("b3"), true, true},
		{"ancestor turn only after the fork", "lineage", own("mid"), true, true},
		{"ancestor turn before the fork", "lineage", own(root), false, true},
		{"branch off the active lineage", "lineage", own("side"), true, true},
		{"only a rolled-back turn", "lineage", own("b7"), false, true},
		{"only a running turn", "lineage", own("b8"), false, true},
		{"neither a switch nor an own boundary", "lineage",
			domain.ConversationBranch{ID: "side", ParentBranchID: root, SessionID: "someone-else"}, false, false},
		{"switch branch in a missing conversation", "missing",
			domain.ConversationBranch{ID: "act", Strategy: domain.ConversationBranchStrategyApproximateContext, HandoffNote: "note"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counting := &countingBranchStore{Store: st}
			// A snapshot reader is wired, as in production; it must stay unused.
			s := &Service{sessions: counting, reader: SnapshotReaderFunc(func(ctx context.Context, id string) (ConversationRows, error) {
				rows, err := counting.LoadConversationSnapshot(ctx, id)
				return ConversationRows{Turns: rows.Turns}, err
			})}
			if got := s.branchUnanswered(ctx, session, tc.conversation, tc.branch); got != tc.want {
				t.Fatalf("branchUnanswered = %v, want %v", got, tc.want)
			}
			if counting.snapshots != 0 {
				t.Fatalf("loaded the conversation snapshot %d times", counting.snapshots)
			}
			if wantPoints := map[bool]int{true: 1, false: 0}[tc.queries]; counting.points != wantPoints {
				t.Fatalf("point queries = %d, want %d", counting.points, wantPoints)
			}

			// The answer the full snapshot gave before.
			gated := tc.queries
			snapshot, err := st.LoadConversationSnapshot(ctx, tc.conversation)
			before := gated && err == nil
			for _, turn := range snapshot.Turns {
				if turn.BranchID == tc.branch.ID {
					before = false
				}
			}
			if tc.want != before {
				t.Fatalf("snapshot semantics gave %v, point query %v", before, tc.want)
			}
		})
	}
}
