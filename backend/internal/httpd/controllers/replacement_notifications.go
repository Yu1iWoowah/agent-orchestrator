package controllers

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

func replacementNoticeViews(op domain.OrchestratorReplacement, receipts []domain.ReplacementNoticeReceipt) ([]ReplacementNotificationView, error) {
	if len(receipts) != len(op.Notifications) {
		return nil, domain.ErrReplacementStateInvalid
	}
	targets := make(map[domain.SessionID]domain.ReplacementWorkerNotification, len(op.Notifications))
	for _, target := range op.Notifications {
		targets[target.SessionID] = target
	}
	views := make([]ReplacementNotificationView, 0, len(receipts))
	for _, p := range receipts {
		target, found := targets[p.SessionID]
		if !found || p.OperationID != op.ID || target.ContactID != p.ContactID || !target.CreatedAt.Equal(p.CreatedAt) {
			return nil, domain.ErrReplacementStateInvalid
		}
		delete(targets, p.SessionID)
		v := ReplacementNotificationView{SessionID: p.SessionID, ContactID: p.ContactID, State: string(p.State), Mode: p.Mode, CreatedAt: p.CreatedAt, AttemptStartedAt: p.AttemptedAt, SettledAt: p.SettledAt, EvidenceKind: p.EvidenceKind, ErrorCode: noticePublicErrorCode(p.ErrorCode)}
		switch p.State {
		case domain.ReplacementNoticePending, domain.ReplacementNoticeSkipped:
		case domain.ReplacementNoticeUncertain:
			if p.AttemptID != "" && p.OutcomeRecordedAt == nil && v.ErrorCode == "" {
				v.ErrorCode = "DELIVERY_IN_FLIGHT"
			}
		case domain.ReplacementNoticeAccepted:
			if p.Mode == domain.SessionModeChat {
				v.ConversationID, v.MessageID, v.TurnID = p.ConversationID, p.MessageID, p.TurnID
			}
		default:
			return nil, domain.ErrReplacementStateInvalid
		}
		views = append(views, v)
	}
	return views, nil
}
func noticePublicErrorCode(code string) string {
	switch code {
	case "", "DELIVERY_NOT_ATTEMPTED", "DELIVERY_UNCONFIRMED", "TARGET_TERMINATED", "DELIVERY_RETRY_CAPACITY":
		return code
	default:
		return "DELIVERY_EVIDENCE_UNVERIFIED"
	}
}
