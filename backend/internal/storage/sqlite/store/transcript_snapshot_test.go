package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestTranscriptSnapshotIsRawReadOnlyAndSessionScoped(t *testing.T) {
	st, session, conversation := conversationFixture(t)
	ctx := context.Background()
	_, err := st.AppendUserMessage(ctx, conversation, session, "gen-1", domain.ConversationMessage{ID: "export-user", Text: "unaltered", Origin: domain.MessageOriginHuman, ClientMessageID: "export-client"}, "export-turn", histClock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProjectProviderEvent(ctx, conversation, session, "gen-1", "export-event", "turn.started", `{"providerTurnId":"opaque-turn","providerConversationId":"native-root"}`, histClock, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	a, err := st.LoadTranscriptSourceSnapshot(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.LoadTranscriptSourceSnapshot(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint == "" || a.Fingerprint != b.Fingerprint {
		t.Fatal("read changed snapshot")
	}
	if len(a.Messages) != 1 || a.Messages[0].Text != "unaltered" || len(a.NativeThreads) != 1 || a.NativeThreads[0].ProviderTurnID != "opaque-turn" {
		t.Fatalf("lost raw evidence: %+v", a)
	}
	if a.CutoffSequence == 0 || a.Session.ID != session {
		t.Fatal("missing owner/cutoff")
	}
	other := sampleRecord("hist")
	created, err := st.CreateSession(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.LoadTranscriptSourceSnapshot(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Messages) != 0 || len(c.NativeThreads) != 0 {
		t.Fatal("cross-session data")
	}
}

func TestTranscriptSnapshotIgnoresUsageTouchOrdering(t *testing.T) {
	st, session, _ := conversationFixture(t)
	ctx := context.Background()
	for i, id := range []string{"first", "second"} {
		if _, e := st.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: session, Harness: domain.HarnessClaudeCode, NativeRootID: id, UpdatedAt: histClock.Add(time.Duration(i) * time.Second)}); e != nil {
			t.Fatal(e)
		}
	}
	before, e := st.LoadTranscriptSourceSnapshot(ctx, session)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.UpsertUsageBinding(ctx, domain.UsageBindingRecord{SessionID: session, Harness: domain.HarnessClaudeCode, NativeRootID: "first", UpdatedAt: histClock.Add(time.Hour)}); e != nil {
		t.Fatal(e)
	}
	after, e := st.LoadTranscriptSourceSnapshot(ctx, session)
	if e != nil {
		t.Fatal(e)
	}
	if before.Fingerprint != after.Fingerprint {
		t.Fatal("usage timestamp reordered immutable proof")
	}
}
