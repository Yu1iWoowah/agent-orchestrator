package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type questionSemanticSession interface {
	SendSemanticWithOptions(context.Context, domain.SessionID, string, string, ports.MessageDeliveryOptions) error
}

type questionSemanticDelivery struct {
	sessions questionSemanticSession
	now      func() time.Time
}

func (d questionSemanticDelivery) Submit(ctx context.Context, id domain.SessionID, message, key string) error {
	if d.sessions == nil {
		return errors.New("this build does not support semantic delivery of human answers; answer remains saved")
	}
	return d.sessions.SendSemanticWithOptions(ctx, id, message, key, ports.MessageDeliveryOptions{AuthoredByUser: true, InteractionAt: d.now().UTC()})
}
