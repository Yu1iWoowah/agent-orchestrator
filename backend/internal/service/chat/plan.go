package chat

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type conversationPlanReader interface {
	ConversationPlans(context.Context, string, int64, int64) ([]domain.ConversationPlanVersion, error)
}

// PlanSnapshot is independent of the transcript's loaded window. Version nil
// means absent; a non-nil version with no steps is an explicit clear.
type PlanSnapshot struct {
	ConversationID string
	ActiveBranchID string
	Version        *domain.ConversationPlanVersion
}

type PlanHistory struct {
	ConversationID     string
	ActiveBranchID     string
	Versions           []domain.ConversationPlanVersion
	HasMoreBefore      bool
	NextBeforeSequence int64
}

func (s *Service) Plan(ctx context.Context, id domain.SessionID) (PlanSnapshot, error) {
	conversation, plans, err := s.readPlans(ctx, id, 0, 1)
	if err != nil {
		return PlanSnapshot{}, err
	}
	result := PlanSnapshot{ConversationID: conversation.ID, ActiveBranchID: conversation.ActiveBranchID}
	if len(plans) > 0 {
		result.Version = &plans[0]
	}
	return result, nil
}

func (s *Service) PlanHistory(ctx context.Context, id domain.SessionID, beforeSequence, limit int64) (PlanHistory, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	conversation, plans, err := s.readPlans(ctx, id, beforeSequence, limit+1)
	if err != nil {
		return PlanHistory{}, err
	}
	result := PlanHistory{ConversationID: conversation.ID, ActiveBranchID: conversation.ActiveBranchID,
		Versions: plans, HasMoreBefore: int64(len(plans)) > limit}
	if result.HasMoreBefore {
		result.Versions = plans[:limit]
		result.NextBeforeSequence = result.Versions[len(result.Versions)-1].Sequence
	}
	return result, nil
}

func (s *Service) readPlans(ctx context.Context, id domain.SessionID, beforeSequence, limit int64) (domain.ConversationRecord, []domain.ConversationPlanVersion, error) {
	if _, err := s.requireChatSession(ctx, id); err != nil {
		return domain.ConversationRecord{}, nil, err
	}
	reader, ok := s.store.(conversationPlanReader)
	if !ok {
		return domain.ConversationRecord{}, nil, fmt.Errorf("%w: durable plans", ports.ErrChatUnsupported)
	}
	for range 2 {
		conversation, err := s.store.ConversationForSession(ctx, id)
		if errors.Is(err, domain.ErrNoConversation) {
			return domain.ConversationRecord{}, []domain.ConversationPlanVersion{}, nil
		}
		if err != nil {
			return domain.ConversationRecord{}, nil, err
		}
		plans, err := reader.ConversationPlans(ctx, conversation.ID, beforeSequence, limit)
		if err != nil {
			return domain.ConversationRecord{}, nil, err
		}
		current, err := s.store.ConversationForSession(ctx, id)
		if err != nil {
			return domain.ConversationRecord{}, nil, err
		}
		if current.ID == conversation.ID && current.ActiveBranchID == conversation.ActiveBranchID && current.SessionID == conversation.SessionID {
			return conversation, plans, nil
		}
	}
	return domain.ConversationRecord{}, nil, ErrControllerHandoff
}
