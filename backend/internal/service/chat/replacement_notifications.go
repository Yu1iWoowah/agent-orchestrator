package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// DeliverReplacementChatWorkerFYI only admits an existing live controller.
// Send queues automation behind the current turn; it never steers, wakes,
// interrupts, or stops it. Canonical client IDs recover lost response receipts.
func (s *Service) DeliverReplacementChatWorkerFYI(ctx context.Context, id string, target domain.SessionID, attempt string, revision int64) error {
	authority, ok := s.store.(ports.ReplacementWorkerNoticeStore)
	if !ok {
		return domain.ErrReplacementStateInvalid
	}
	controller, release, err := s.admitControllerWork(ctx, target, false, controllerIntake)
	if err != nil {
		return err
	}
	defer release()
	op, worker, receipt, err := authority.ValidateReplacementWorkerNotice(ctx, id, target, attempt, revision)
	if err != nil {
		return err
	}
	if receipt.Mode != domain.SessionModeChat || worker.ControllerOwner() != receipt.Owner || worker.Metadata.ControllerGeneration != controller.Generation() || worker.Metadata.ProviderConversationID != controller.ProviderConversationID() || controller.ConversationID() != receipt.ConversationID {
		return domain.ErrReplacementSourceChanged
	}
	_, err = controller.Send(ctx, ports.ChatUserMessage{Text: op.WorkerFYI(), ClientMessageID: receipt.ContactID, Origin: domain.MessageOriginAutomation})
	return err
}
