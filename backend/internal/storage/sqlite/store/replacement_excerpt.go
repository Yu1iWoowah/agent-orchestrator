package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ReplacementExcerptSourceAllowed proves an exact canonical pending payload was
// transferred from this source and that the caller owns the project controller.
// It does not trust captured paired text; Chat rereads/revalidates that source.
func (s *Store) ReplacementExcerptSourceAllowed(ctx context.Context, session domain.SessionID, generation, target, turn, source, hash string) (bool, error) {
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	q := gen.New(tx)
	row, err := q.SelectReplacementExcerptTransfer(ctx, gen.SelectReplacementExcerptTransferParams{TurnID: turn, SourceConversationID: source, TargetConversationID: target, PayloadHash: hash, TargetSessionID: session, TargetGeneration: generation})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	operation, err := q.GetOrchestratorReplacement(ctx, row.OperationID)
	if err != nil {
		return false, err
	}
	op, err := s.decodeCheckpointedReplacement(ctx, q, operation)
	if err != nil {
		return false, err
	}
	if op.Phase != domain.ReplacementNotifying && op.Phase != domain.ReplacementCompleted {
		return false, nil
	}
	writer, destination, err := verifiedReplacementIntakeWriter(ctx, q, op)
	if errors.Is(err, domain.ErrReplacementIntakeFenced) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if writer.ID != session || writer.Metadata.ControllerGeneration != generation || destination.ID != target {
		return false, nil
	}
	current, err := domain.ReplacementIntakePayloadHash(domain.QueuedTurn{TurnID: row.TurnID.String, Text: row.Text, ClientMessageID: row.ClientMessageID, Origin: row.Origin, DeliveryContentJSON: row.DeliveryContentJson, SenderSessionID: row.SenderSessionID, SenderProjectID: row.SenderProjectID, SenderDisplayName: row.SenderDisplayName})
	if err != nil {
		return false, err
	}
	// The query already requires receipt.payload_hash = hash.
	return current == row.PayloadHash, nil
}
