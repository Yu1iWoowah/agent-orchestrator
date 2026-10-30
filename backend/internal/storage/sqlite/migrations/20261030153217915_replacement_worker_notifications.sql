-- +goose Up
CREATE TABLE orchestrator_replacement_notices (
 operation_id TEXT NOT NULL REFERENCES orchestrator_replacements(id),
 session_id TEXT NOT NULL,
 contact_id TEXT NOT NULL UNIQUE,
 state TEXT NOT NULL CHECK (state IN ('pending','accepted','uncertain','skipped')),
 revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
 state_json TEXT NOT NULL CHECK (json_valid(state_json) AND length(CAST(state_json AS BLOB)) <= 8192),
 updated_at TEXT NOT NULL,
 PRIMARY KEY (operation_id, session_id),
 CHECK (json_extract(state_json,'$.operationId') IS operation_id),
 CHECK (json_extract(state_json,'$.sessionId') IS session_id),
 CHECK (json_extract(state_json,'$.contactId') IS contact_id),
 CHECK (json_extract(state_json,'$.state') IS state)
);

CREATE TABLE orchestrator_replacement_notice_history (
 operation_id TEXT NOT NULL,
 session_id TEXT NOT NULL,
 sequence INTEGER NOT NULL CHECK (sequence > 0),
 state_json TEXT NOT NULL CHECK (json_valid(state_json) AND length(CAST(state_json AS BLOB)) <= 8192),
 recorded_at TEXT NOT NULL,
 PRIMARY KEY (operation_id, session_id, sequence),
 FOREIGN KEY (operation_id, session_id) REFERENCES orchestrator_replacement_notices(operation_id, session_id)
);

-- +goose StatementBegin
CREATE TRIGGER replacement_notice_identity BEFORE UPDATE ON orchestrator_replacement_notices
WHEN OLD.operation_id IS NOT NEW.operation_id OR OLD.session_id IS NOT NEW.session_id
 OR OLD.contact_id IS NOT NEW.contact_id OR OLD.state IN ('accepted','skipped')
BEGIN SELECT RAISE(ABORT,'replacement notice identity or settled receipt is immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER replacement_notice_no_delete BEFORE DELETE ON orchestrator_replacement_notices
BEGIN SELECT RAISE(ABORT,'replacement notice receipts cannot be deleted'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER replacement_notice_history_no_update BEFORE UPDATE ON orchestrator_replacement_notice_history
BEGIN SELECT RAISE(ABORT,'replacement notice history is immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER replacement_notice_history_no_delete BEFORE DELETE ON orchestrator_replacement_notice_history
BEGIN SELECT RAISE(ABORT,'replacement notice history cannot be deleted'); END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER replacement_notice_history_no_delete;
DROP TRIGGER replacement_notice_history_no_update;
DROP TRIGGER replacement_notice_no_delete;
DROP TRIGGER replacement_notice_identity;
DROP TABLE orchestrator_replacement_notice_history;
DROP TABLE orchestrator_replacement_notices;
