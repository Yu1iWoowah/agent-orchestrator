-- +goose Up
CREATE TABLE session_agent_settings_writer_settlements (
    operation_id TEXT NOT NULL REFERENCES session_agent_settings_operations(id) ON DELETE CASCADE,
    operation_revision INTEGER NOT NULL CHECK (operation_revision >= 0),
    actor_kind TEXT NOT NULL DEFAULT 'local_operator_unverified' CHECK (actor_kind = 'local_operator_unverified'),
    recorded_at TIMESTAMP NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(operation_id, operation_revision)
);

-- +goose Down
-- Retain operator stop evidence rather than silently discard history.
CREATE TEMP TABLE writer_settlement_downgrade_guard (ok INTEGER CHECK (ok=1));
INSERT INTO writer_settlement_downgrade_guard SELECT 0 FROM session_agent_settings_writer_settlements LIMIT 1;
DROP TABLE writer_settlement_downgrade_guard;
DROP TABLE session_agent_settings_writer_settlements;
