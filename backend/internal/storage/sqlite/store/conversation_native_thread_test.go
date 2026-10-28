package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestConversationNativeThreadEvidenceRequiresArchivedThreadIdentity(t *testing.T) {
	st, session, conversation := conversationFixture(t)
	ctx := context.Background()
	for i, event := range []struct{ method, payload string }{
		{"turn.started", `{"providerTurnId":"root-turn","providerConversationId":"root-thread"}`},
		{"turn.completed", `{"providerTurnId":"root-turn","providerConversationId":"root-thread"}`},
		{"turn.started", `{"providerTurnId":"child-turn","providerConversationId":"child-thread"}`},
		{"turn.completed", `{"providerTurnId":"child-turn","providerConversationId":"conflicting-thread"}`},
		{"turn.completed", `{"providerTurnId":"unknown-turn"}`},
		{"turn.completed", `{"providerTurnId":"blank-turn","providerConversationId":""}`},
		{"turn.completed", `{"providerTurnId":"invalid-turn","providerConversationId":7}`},
		{"message.completed", `{"providerTurnId":"message-turn","providerConversationId":"root-thread"}`},
	} {
		if _, err := st.ProjectProviderEvent(ctx, conversation, session, "gen-1", fmt.Sprintf("event-%d", i), event.method, event.payload, histClock, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	evidence, err := st.ConversationNativeThreadEvidence(ctx, conversation, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 3 {
		t.Fatalf("evidence = %+v, want one root identity and both conflicting child identities", evidence)
	}
	seen := map[string]bool{}
	for _, item := range evidence {
		if item.BranchID != conversation+":root" {
			t.Fatalf("lost branch scope: %+v", item)
		}
		seen[item.ProviderTurnID+":"+item.ProviderConversationID] = true
	}
	for _, key := range []string{"root-turn:root-thread", "child-turn:child-thread", "child-turn:conflicting-thread"} {
		if !seen[key] {
			t.Fatalf("missing %s in %v", key, seen)
		}
	}
	for _, scope := range []struct{ conversation, session string }{
		{"another-conversation", string(session)},
		{conversation, "another-session"},
	} {
		rows, err := st.ConversationNativeThreadEvidence(ctx, scope.conversation, domain.SessionID(scope.session))
		if err != nil || len(rows) != 0 {
			t.Fatalf("cross-owner evidence: %+v, %v", rows, err)
		}
	}
}
