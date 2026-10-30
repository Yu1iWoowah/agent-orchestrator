package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementControlContextKey struct{}
type replacementControlGrant struct {
	session    domain.SessionID
	generation string
	clientID   string
	promptHash string
}

func replacementPromptHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// A renderer or relay may supply client IDs but cannot supply this private
// context grant. Enforce it at both durable queue entry points.
func validateReplacementControlMessage(ctx context.Context, id domain.SessionID, generation string, msg ports.ChatUserMessage) error {
	if !strings.HasPrefix(msg.ClientMessageID, "replacement:") && !strings.HasPrefix(msg.ClientMessageID, "replacement/") {
		return nil
	}
	grant, ok := ctx.Value(replacementControlContextKey{}).(replacementControlGrant)
	if !ok || grant.session != id || grant.generation != generation || grant.clientID != msg.ClientMessageID || grant.promptHash != replacementPromptHash(msg.Text) || msg.Origin != domain.MessageOriginAutomation || msg.AuthoredByUser || msg.SenderSessionID != "" || len(msg.Content) != 0 || len(msg.Excerpts) != 0 || msg.Continuation {
		return domain.ErrReplacementIntakeFenced
	}
	return nil
}
