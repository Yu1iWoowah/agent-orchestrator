package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// restoreReplacementHumanProjection runs after the durable hold is released in
// the same transaction. An accepted queued prompt is legacy/unconfirmed native
// history, exactly like ordinary human intake; it never mints a completed turn.
func restoreReplacementHumanProjection(ctx context.Context, q *gen.Queries, id domain.SessionID) error {
	candidate, err := q.SelectReplacementHumanProjection(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !candidate.TurnID.Valid || candidate.InteractionAt.IsZero() {
		return domain.ErrReplacementStateInvalid
	}
	_, err = q.RecordSessionHumanMessage(ctx, gen.RecordSessionHumanMessageParams{ID: id, TurnID: candidate.TurnID.String, LatestUserPrompt: candidate.Text, LatestUserPromptAt: timeToNullTime(candidate.InteractionAt)})
	if err != nil {
		return fmt.Errorf("restore released human intake projection: %w", err)
	}
	return nil
}
