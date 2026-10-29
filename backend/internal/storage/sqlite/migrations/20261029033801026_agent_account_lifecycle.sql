-- +goose Up
ALTER TABLE agent_accounts ADD COLUMN label TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_accounts ADD COLUMN lifecycle_operation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_accounts ADD COLUMN disabled_at TIMESTAMP;
ALTER TABLE agent_accounts ADD COLUMN retired_at TIMESTAMP;
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
CREATE UNIQUE INDEX agent_account_operation_active ON agent_account_operations(account_id) WHERE phase <> 'complete';
-- Match global model-catalog CDC: project-scoped session_updated with a typed kind.
-- Installations without projects use account polling until a project exists.
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
-- +goose StatementBegin
CREATE TRIGGER agent_accounts_cdc_update AFTER UPDATE ON agent_accounts BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT projects.id,NULL,'session_updated',json_object('kind','agent_accounts','accountId',NEW.id),strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM projects WHERE archived_at IS NULL;
END;
-- +goose StatementEnd

-- +goose Down
-- Recovery facts cannot be erased by a schema rollback after lifecycle use.
-- Roll back application entry points instead, preserving this schema and data.
CREATE TEMP TABLE account_lifecycle_rollback_guard(unused INTEGER CHECK(unused=1));
INSERT INTO account_lifecycle_rollback_guard(unused)
SELECT CASE WHEN EXISTS(SELECT 1 FROM agent_account_operations)
 OR EXISTS(SELECT 1 FROM agent_accounts WHERE label<>'' OR lifecycle_operation_id<>'' OR disabled_at IS NOT NULL OR retired_at IS NOT NULL)
 THEN 0 ELSE 1 END;
DROP TABLE account_lifecycle_rollback_guard;
DROP TRIGGER agent_accounts_cdc_update;
DROP TRIGGER agent_account_operations_cdc_update;
DROP TRIGGER agent_account_operations_cdc_insert;
DROP TABLE agent_account_operations;
ALTER TABLE agent_accounts DROP COLUMN retired_at;
ALTER TABLE agent_accounts DROP COLUMN disabled_at;
ALTER TABLE agent_accounts DROP COLUMN lifecycle_operation_id;
ALTER TABLE agent_accounts DROP COLUMN label;
