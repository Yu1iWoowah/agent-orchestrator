-- +goose Up
-- +goose StatementBegin
-- Lineage reads match messages/activities by turn_id below a fork sequence.
-- Without statistics the planner prefers (conversation_id, sequence), which
-- makes resume quadratic in history; these covering indexes keep it linear.
CREATE INDEX idx_conversation_messages_turn ON conversation_messages (conversation_id, turn_id, sequence);
CREATE INDEX idx_conversation_activities_turn ON conversation_activities (conversation_id, turn_id, sequence);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_conversation_activities_turn;
DROP INDEX IF EXISTS idx_conversation_messages_turn;
-- +goose StatementEnd
