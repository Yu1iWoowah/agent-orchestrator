package chat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type conversationAttentionStore interface {
	ConversationAttention(context.Context, domain.SessionID, string, domain.ConversationAttentionQuery) (domain.ConversationAttentionPage, error)
	MarkConversationAttentionRead(context.Context, domain.SessionID, string, string, string, int64, time.Time) error
}

// Attention reads provider facts independently of transcript pagination, without
// waking or mutating the provider. The snapshot branch fences counts as well as
// rows, including an empty page during A -> B -> A transitions.
func (s *Service) Attention(ctx context.Context, id domain.SessionID, query domain.ConversationAttentionQuery) (domain.ConversationAttentionPage, error) {
	if _, err := s.requireChatSession(ctx, id); err != nil {
		return domain.ConversationAttentionPage{}, err
	}
	reader, ok := s.store.(conversationAttentionStore)
	if !ok {
		return domain.ConversationAttentionPage{}, fmt.Errorf("%w: attention index", ports.ErrChatUnsupported)
	}
	for range 2 {
		conversation, err := s.store.ConversationForSession(ctx, id)
		if errors.Is(err, domain.ErrNoConversation) {
			return domain.ConversationAttentionPage{Items: []domain.ConversationAttentionItem{}}, nil
		}
		if err != nil {
			return domain.ConversationAttentionPage{}, err
		}
		page, err := reader.ConversationAttention(ctx, id, conversation.ID, query)
		if errors.Is(err, domain.ErrNoConversation) {
			continue
		}
		if err != nil {
			return domain.ConversationAttentionPage{}, err
		}
		current, err := s.store.ConversationForSession(ctx, id)
		if err != nil {
			return domain.ConversationAttentionPage{}, err
		}
		if current.ID == conversation.ID && conversation.ID == page.ConversationID && current.ActiveBranchID == conversation.ActiveBranchID && conversation.ActiveBranchID == page.ActiveBranchID && current.SessionID == id {
			return page, nil
		}
	}
	return domain.ConversationAttentionPage{}, ErrControllerHandoff
}

// MarkAttentionRead records an explicit seen revision through the source owner.
func (s *Service) MarkAttentionRead(ctx context.Context, id domain.SessionID, branchID, activityID string, revision int64) error {
	if _, err := s.requireChatSession(ctx, id); err != nil {
		return err
	}
	writer, ok := s.store.(conversationAttentionStore)
	if !ok {
		return fmt.Errorf("%w: attention read state", ports.ErrChatUnsupported)
	}
	conversation, err := s.store.ConversationForSession(ctx, id)
	if errors.Is(err, domain.ErrNoConversation) {
		return domain.ErrAttentionChanged
	}
	if err != nil {
		return err
	}
	return writer.MarkConversationAttentionRead(ctx, id, conversation.ID, branchID, activityID, revision, s.now())
}
