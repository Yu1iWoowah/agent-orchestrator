package chat

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementExcerptReader interface {
	ReplacementExcerptSourceAllowed(context.Context, domain.SessionID, string, string, string, string, string) (bool, error)
}

func hydrateQueuedExcerptReferences(ctx context.Context, c *Controller, msg *ports.ChatUserMessage, queued domain.QueuedTurn) error {
	if len(msg.Excerpts) == 0 {
		return nil
	}
	source := msg.Excerpts[0].ConversationID
	if err := validateExcerptReferences(msg.Excerpts, source); err != nil {
		return err
	}
	if source != c.conversation.ID {
		reader, ok := c.store.(replacementExcerptReader)
		if !ok {
			return fmt.Errorf("%w: no verified intake transfer for source conversation", ErrExcerptInvalid)
		}
		hash, err := domain.ReplacementIntakePayloadHash(queued)
		if err != nil {
			return err
		}
		allowed, err := reader.ReplacementExcerptSourceAllowed(ctx, c.sessionID, c.generation, c.conversation.ID, queued.TurnID, source, hash)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("%w: transferred intake or project controller changed", ErrExcerptInvalid)
		}
	}
	return hydrateExcerptConversation(ctx, c.store, source, msg)
}
