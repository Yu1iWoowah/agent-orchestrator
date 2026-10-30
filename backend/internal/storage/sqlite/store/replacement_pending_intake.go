package store

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ReadReplacementIntake inventories canonical pending rows after any frozen
// cutoff. One read snapshot preserves claims and never certifies native delivery.
func (s *Store) ReadReplacementIntake(ctx context.Context, id string) (domain.ReplacementIntake, error) {
	var out domain.ReplacementIntake
	err := s.replacementRead(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(ctx, id)
		if err != nil {
			return fmt.Errorf("read replacement intake operation: %w", err)
		}
		op, err := s.decodeCheckpointedReplacement(ctx, q, row)
		if err != nil {
			return err
		}
		out = domain.ReplacementIntake{OperationID: op.ID, ProjectID: op.ProjectID, SourceSessionID: op.SourceSessionID, SuccessorSessionID: op.SuccessorSessionID, Phase: op.Phase, IntakeRevision: op.IntakeRevision, Messages: []domain.ReplacementIntakeMessage{}, Reports: []domain.ReplacementIntakeReport{}}
		messages, err := q.SelectReplacementPendingMessages(ctx, id)
		if err != nil {
			return fmt.Errorf("read replacement queued messages: %w", err)
		}
		for _, m := range messages {
			out.Messages = append(out.Messages, domain.ReplacementIntakeMessage{ID: m.ID, ConversationID: m.ConversationID, TurnID: m.TurnID.String, Sequence: m.Sequence, ClientMessageID: m.ClientMessageID, Text: m.Text, DeliveryContentJSON: m.DeliveryContentJson, Origin: m.Origin, SenderSessionID: m.SenderSessionID, SenderProjectID: m.SenderProjectID, SenderDisplayName: m.SenderDisplayName, RequestedAt: m.RequestedAt, ProviderReserved: m.ProviderTurnID != "" || m.ProviderInputText != "" || m.PromotionStartedAt.Valid || m.StartedAt.Valid || m.CompletedAt.Valid || m.BatchedInput != 0 || m.PromotedToTurnID.Valid, NativeDelivery: "unconfirmed"})
		}
		reports, err := q.ListReportsByProject(ctx, string(op.ProjectID))
		if err != nil {
			return fmt.Errorf("read replacement pending reports: %w", err)
		}
		for _, row := range reports {
			report := reportFromGen(row)
			if report.SessionID == op.SourceSessionID || report.SessionID == op.SuccessorSessionID || (report.DeliveryState != domain.ReportPending && report.DeliveryState != domain.ReportClaimed) {
				continue
			}
			pending := domain.ReplacementIntakeReport{ID: report.ID, SessionID: report.SessionID, State: report.State, Note: report.Note, Message: report.Message, CreatedAt: report.CreatedAt, DeliveryState: report.DeliveryState, RepeatCount: report.RepeatCount, Outputs: []domain.ReplacementIntakeOutput{}}
			outputs, err := q.ListReportOutputs(ctx, report.ID)
			if err != nil {
				return fmt.Errorf("read replacement report output: %w", err)
			}
			for _, output := range outputs {
				pending.Outputs = append(pending.Outputs, domain.ReplacementIntakeOutput{Kind: domain.ReportOutputKind(output.Kind), Reference: output.Reference, Label: output.Label})
			}
			out.Reports = append(out.Reports, pending)
		}
		return nil
	})
	return out, err
}
