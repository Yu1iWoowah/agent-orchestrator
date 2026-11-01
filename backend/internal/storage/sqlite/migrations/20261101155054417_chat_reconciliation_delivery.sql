-- +goose Up

-- Settings work and delivery evidence are independent journals. No turn/held
-- table is rebuilt here. Disposition is deliberately extensible for PR3-S.
CREATE TABLE agent_settings_reconciliations (
 id TEXT PRIMARY KEY,
 sequence INTEGER NOT NULL UNIQUE,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 owner_generation TEXT NOT NULL,
 settings_revision INTEGER NOT NULL CHECK (settings_revision >= 0),
 phase TEXT NOT NULL CHECK (length(phase) BETWEEN 1 AND 64),
 version INTEGER NOT NULL CHECK (version >= 0),
 executor_id TEXT NOT NULL,
 claim_until TIMESTAMP NOT NULL,
 record_json TEXT NOT NULL CHECK (json_valid(record_json)),
 created_at TIMESTAMP NOT NULL,
 updated_at TIMESTAMP NOT NULL
);
CREATE INDEX agent_settings_reconciliations_session ON agent_settings_reconciliations(session_id,sequence DESC);
CREATE UNIQUE INDEX agent_settings_reconciliations_active ON agent_settings_reconciliations(session_id,owner_generation,settings_revision)
 WHERE phase NOT IN ('ready','cancelled');

CREATE TABLE chat_delivery_attempts (
 id TEXT PRIMARY KEY,
 sequence INTEGER NOT NULL UNIQUE,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 conversation_id TEXT NOT NULL,
 owner_generation TEXT NOT NULL,
 disposition TEXT NOT NULL CHECK (length(disposition) BETWEEN 1 AND 64),
 evidence_level INTEGER NOT NULL DEFAULT 0 CHECK (evidence_level BETWEEN 0 AND 2),
 version INTEGER NOT NULL CHECK (version >= 0),
 record_json TEXT NOT NULL CHECK (json_valid(record_json)),
 created_at TIMESTAMP NOT NULL,
 updated_at TIMESTAMP NOT NULL
);
CREATE INDEX chat_delivery_attempts_session ON chat_delivery_attempts(session_id,sequence);
-- Turn IDs are opaque references rather than FKs to the table S0 rebuilds.
-- Reservation validates their owner/branch and existence in its transaction.
CREATE TABLE chat_delivery_attempt_members (
 attempt_id TEXT NOT NULL REFERENCES chat_delivery_attempts(id) ON DELETE CASCADE,
 turn_id TEXT NOT NULL,
 ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
 PRIMARY KEY(attempt_id,turn_id),
 UNIQUE(attempt_id,ordinal)
);
CREATE INDEX chat_delivery_attempt_members_turn ON chat_delivery_attempt_members(turn_id,attempt_id);
CREATE TABLE chat_delivery_receipts (
 attempt_id TEXT NOT NULL REFERENCES chat_delivery_attempts(id) ON DELETE CASCADE,
 version INTEGER NOT NULL CHECK (version > 0),
 receipt_json TEXT NOT NULL CHECK (json_valid(receipt_json)),
 recorded_at TIMESTAMP NOT NULL,
 PRIMARY KEY(attempt_id,version)
);

-- +goose StatementBegin
CREATE TRIGGER agent_settings_reconciliation_insert_cdc AFTER INSERT ON agent_settings_reconciliations
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,id,'session_updated',json_object('id',id),CURRENT_TIMESTAMP FROM sessions WHERE id=NEW.session_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER agent_settings_reconciliation_update_cdc AFTER UPDATE ON agent_settings_reconciliations
WHEN NEW.version <> OLD.version
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,id,'session_updated',json_object('id',id),CURRENT_TIMESTAMP FROM sessions WHERE id=NEW.session_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER chat_delivery_attempt_update_cdc AFTER UPDATE ON chat_delivery_attempts
WHEN NEW.version <> OLD.version
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,id,'session_updated',json_object('id',id),CURRENT_TIMESTAMP FROM sessions WHERE id=NEW.session_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER chat_delivery_attempt_insert_cdc AFTER INSERT ON chat_delivery_attempts
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,id,'session_updated',json_object('id',id),CURRENT_TIMESTAMP FROM sessions WHERE id=NEW.session_id;
END;
-- +goose StatementEnd


CREATE TABLE chat_wake_attempts (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 version INTEGER NOT NULL,
 record_json TEXT NOT NULL CHECK(json_valid(record_json))
);
CREATE INDEX chat_wake_attempt_session ON chat_wake_attempts(session_id,sequence DESC);
-- +goose StatementBegin
CREATE TRIGGER chat_wake_attempt_insert_cdc AFTER INSERT ON chat_wake_attempts
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,id,'session_updated',json_object('id',id),CURRENT_TIMESTAMP FROM sessions WHERE id=NEW.session_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER chat_wake_attempt_update_cdc AFTER UPDATE ON chat_wake_attempts
WHEN NEW.version <> OLD.version
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,id,'session_updated',json_object('id',id),CURRENT_TIMESTAMP FROM sessions WHERE id=NEW.session_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER chat_wake_attempt_update_cdc;
DROP TRIGGER chat_wake_attempt_insert_cdc;
DROP TABLE chat_wake_attempts;
DROP TRIGGER chat_delivery_attempt_insert_cdc;
DROP TRIGGER chat_delivery_attempt_update_cdc;
DROP TRIGGER agent_settings_reconciliation_update_cdc;
DROP TRIGGER agent_settings_reconciliation_insert_cdc;
DROP TABLE chat_delivery_receipts;
DROP TABLE chat_delivery_attempt_members;
DROP TABLE chat_delivery_attempts;
DROP TABLE agent_settings_reconciliations;
