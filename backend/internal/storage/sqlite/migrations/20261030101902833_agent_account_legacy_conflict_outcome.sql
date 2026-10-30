-- +goose Up
-- Service acceptance already records this outcome. Preserve all recovery facts
-- while extending the CHECK; install CDC after the copy so it emits no fake events.
DROP TRIGGER agent_account_operations_cdc_insert;
DROP TRIGGER agent_account_operations_cdc_update;
ALTER TABLE agent_account_operations RENAME TO agent_account_operations_previous;
CREATE TABLE agent_account_operations (
 id TEXT PRIMARY KEY CHECK(trim(id) <> ''),
 account_id TEXT NOT NULL,
 provider TEXT NOT NULL CHECK(provider IN ('claude','codex')),
 action TEXT NOT NULL CHECK(action IN ('login','login-again','logout','remove')),
 phase TEXT NOT NULL CHECK(phase IN ('pending','accepted','closing','logging-out','deleting','complete')),
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','complete','cancelled','duplicate','mismatch','removed','signed-out','legacy-conflict')),
 reason_code TEXT NOT NULL DEFAULT '' CHECK(reason_code IN ('','account_cleanup_unconfirmed','account_storage_unsafe','account_storage_unavailable','account_operation_busy')),
 terminal_id TEXT NOT NULL DEFAULT '',
 result_account_id TEXT NOT NULL DEFAULT '',
 was_disabled BOOLEAN NOT NULL DEFAULT FALSE,
 created_at TIMESTAMP NOT NULL,
 updated_at TIMESTAMP NOT NULL
);
INSERT INTO agent_account_operations SELECT * FROM agent_account_operations_previous;
DROP TABLE agent_account_operations_previous;
CREATE UNIQUE INDEX agent_account_operation_active ON agent_account_operations(account_id) WHERE phase <> 'complete';
-- +goose StatementBegin
CREATE TRIGGER agent_account_operations_cdc_insert AFTER INSERT ON agent_account_operations BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT projects.id,NULL,'session_updated',json_object('kind','agent_accounts','operationId',NEW.id,'accountId',NEW.account_id),strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM projects WHERE archived_at IS NULL;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER agent_account_operations_cdc_update AFTER UPDATE ON agent_account_operations BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT projects.id,NULL,'session_updated',json_object('kind','agent_accounts','operationId',NEW.id,'accountId',NEW.account_id),strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM projects WHERE archived_at IS NULL;
END;
-- +goose StatementEnd

-- +goose Down
-- Older binaries cannot represent this outcome; never rewrite or discard it.
CREATE TEMP TABLE account_legacy_conflict_rollback_guard(unused INTEGER CHECK(unused=1));
INSERT INTO account_legacy_conflict_rollback_guard(unused)
SELECT CASE WHEN EXISTS(SELECT 1 FROM agent_account_operations WHERE outcome='legacy-conflict') THEN 0 ELSE 1 END;
DROP TABLE account_legacy_conflict_rollback_guard;
DROP TRIGGER agent_account_operations_cdc_insert;
DROP TRIGGER agent_account_operations_cdc_update;
ALTER TABLE agent_account_operations RENAME TO agent_account_operations_previous;
CREATE TABLE agent_account_operations (
 id TEXT PRIMARY KEY CHECK(trim(id) <> ''),
 account_id TEXT NOT NULL,
 provider TEXT NOT NULL CHECK(provider IN ('claude','codex')),
 action TEXT NOT NULL CHECK(action IN ('login','login-again','logout','remove')),
 phase TEXT NOT NULL CHECK(phase IN ('pending','accepted','closing','logging-out','deleting','complete')),
 outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','complete','cancelled','duplicate','mismatch','removed','signed-out')),
 reason_code TEXT NOT NULL DEFAULT '' CHECK(reason_code IN ('','account_cleanup_unconfirmed','account_storage_unsafe','account_storage_unavailable','account_operation_busy')),
 terminal_id TEXT NOT NULL DEFAULT '',
 result_account_id TEXT NOT NULL DEFAULT '',
 was_disabled BOOLEAN NOT NULL DEFAULT FALSE,
 created_at TIMESTAMP NOT NULL,
 updated_at TIMESTAMP NOT NULL
);
INSERT INTO agent_account_operations SELECT * FROM agent_account_operations_previous;
DROP TABLE agent_account_operations_previous;
CREATE UNIQUE INDEX agent_account_operation_active ON agent_account_operations(account_id) WHERE phase <> 'complete';
-- +goose StatementBegin
CREATE TRIGGER agent_account_operations_cdc_insert AFTER INSERT ON agent_account_operations BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT projects.id,NULL,'session_updated',json_object('kind','agent_accounts','operationId',NEW.id,'accountId',NEW.account_id),strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM projects WHERE archived_at IS NULL;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER agent_account_operations_cdc_update AFTER UPDATE ON agent_account_operations BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT projects.id,NULL,'session_updated',json_object('kind','agent_accounts','operationId',NEW.id,'accountId',NEW.account_id),strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM projects WHERE archived_at IS NULL;
END;
-- +goose StatementEnd
