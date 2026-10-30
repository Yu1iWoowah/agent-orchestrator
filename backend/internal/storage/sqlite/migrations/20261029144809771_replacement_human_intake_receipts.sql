-- +goose Up
-- Keep intake-only human authorship/time while replacement holds projection.
-- Payload remains in the canonical message; deletion removes its projection fact.
CREATE TABLE replacement_human_intake_receipts (
 message_id TEXT PRIMARY KEY REFERENCES conversation_messages(id) ON DELETE CASCADE,
 interaction_at DATETIME NOT NULL
);

-- +goose Down
DROP TABLE replacement_human_intake_receipts;
