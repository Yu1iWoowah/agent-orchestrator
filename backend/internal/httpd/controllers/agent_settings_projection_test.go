package controllers_test

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func TestTurnSettingsResponseOmitsInternalOwnerWithoutChangingReceipt(t *testing.T) {
	owner := domain.AgentSettingsOwner{SessionID: "worker", AccountID: "private-account", ControllerGeneration: "g", ProviderConversationID: "private-native", BranchID: "branch"}
	revision := int64(7)
	receipt := &domain.AgentSettingsTurnSnapshot{State: domain.AgentSettingKnown, Revision: &revision, Owner: &owner}
	body := conversationSnapshotBody(t, chatsvc.Snapshot{Turns: []domain.ConversationTurn{{ID: "turn", AgentSettings: receipt}}})
	turns := body["turns"].([]any)
	settings := turns[0].(map[string]any)["agentSettings"].(map[string]any)
	if _, ok := settings["owner"]; ok {
		t.Fatal("renderer received internal account/native owner")
	}
	if settings["revision"] != float64(7) || receipt.Owner == nil || *receipt.Owner != owner {
		t.Fatal("public projection changed durable receipt or lost revision")
	}
}
