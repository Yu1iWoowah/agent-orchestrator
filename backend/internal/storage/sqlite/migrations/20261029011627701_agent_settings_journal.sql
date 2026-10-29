-- +goose Up
ALTER TABLE session_agent_settings_operations ADD COLUMN apply_mode TEXT NOT NULL DEFAULT 'after-turn' CHECK (apply_mode IN ('after-turn','restart'));
ALTER TABLE session_agent_settings_operations ADD COLUMN error_category TEXT NOT NULL DEFAULT '' CHECK (error_category IN ('','conflict','cancelled','provider','storage','validation','recovery_required'));
ALTER TABLE session_agent_settings_operations ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_agent_settings_operations ADD COLUMN provider_conversation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_agent_settings_operations ADD COLUMN branch_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_agent_settings_operations ADD COLUMN checkpoint TEXT NOT NULL DEFAULT 'queued' CHECK (checkpoint IN ('queued','applying_model','applying_effort','applying_fast','verifying','verified','rolling_back','restored'));
ALTER TABLE session_agent_settings_operations ADD COLUMN operation_revision INTEGER NOT NULL DEFAULT 0 CHECK (operation_revision >= 0);
ALTER TABLE session_agent_settings_operations ADD COLUMN started_at TIMESTAMP;
ALTER TABLE session_agent_settings_operations ADD COLUMN finished_at TIMESTAMP;
ALTER TABLE session_agent_settings_operations ADD COLUMN recovery_json TEXT NOT NULL DEFAULT '' CHECK (recovery_json='' OR json_valid(recovery_json));
ALTER TABLE session_agent_settings_audit ADD COLUMN error_category TEXT NOT NULL DEFAULT '';
ALTER TABLE session_agent_settings_audit ADD COLUMN request_id TEXT NOT NULL DEFAULT '';

-- Checkpoint updates invalidate readers just like phase changes; the original
-- phase trigger remains the sole source for phase CDC.
-- +goose StatementBegin
CREATE TRIGGER session_agent_settings_checkpoint_cdc
AFTER UPDATE OF operation_revision ON session_agent_settings_operations
WHEN NEW.operation_revision <> OLD.operation_revision AND NEW.phase = OLD.phase
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,id,'session_updated',json_object('id',id),CURRENT_TIMESTAMP FROM sessions WHERE id=NEW.session_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER session_agent_settings_checkpoint_cdc;
ALTER TABLE session_agent_settings_operations DROP COLUMN recovery_json;
ALTER TABLE session_agent_settings_audit DROP COLUMN request_id;
ALTER TABLE session_agent_settings_audit DROP COLUMN error_category;
ALTER TABLE session_agent_settings_operations DROP COLUMN finished_at;
ALTER TABLE session_agent_settings_operations DROP COLUMN started_at;
ALTER TABLE session_agent_settings_operations DROP COLUMN operation_revision;
ALTER TABLE session_agent_settings_operations DROP COLUMN checkpoint;
ALTER TABLE session_agent_settings_operations DROP COLUMN branch_id;
ALTER TABLE session_agent_settings_operations DROP COLUMN provider_conversation_id;
ALTER TABLE session_agent_settings_operations DROP COLUMN request_id;
ALTER TABLE session_agent_settings_operations DROP COLUMN error_category;
ALTER TABLE session_agent_settings_operations DROP COLUMN apply_mode;
