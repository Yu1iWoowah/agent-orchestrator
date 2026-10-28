-- +goose Up
ALTER TABLE sessions ADD COLUMN agent_fast_mode TEXT NOT NULL DEFAULT 'default' CHECK (agent_fast_mode IN ('default','on','off'));
ALTER TABLE sessions ADD COLUMN agent_settings_revision INTEGER NOT NULL DEFAULT 0 CHECK (agent_settings_revision >= 0);
ALTER TABLE conversations ADD COLUMN agent_fast_mode TEXT NOT NULL DEFAULT 'default' CHECK (agent_fast_mode IN ('default','on','off'));

-- Chat never persisted a fast choice. Only unambiguous terminal launch choices
-- are evidence of a historical preference; never infer one from a transcript.
-- Skip unchanged rows: even a no-op UPDATE advances the general session revision.
UPDATE sessions SET agent_fast_mode = CASE
    WHEN json_type(launch_options,'$.fastMode') = 'true' THEN 'on'
    ELSE 'off' END
WHERE session_mode = 'tui' AND CASE WHEN json_valid(launch_options) THEN
    (json_type(launch_options,'$.fastMode') = 'true' AND (json_type(launch_options,'$.fastModeOff') IS NULL OR json_type(launch_options,'$.fastModeOff') = 'false'))
    OR (json_type(launch_options,'$.fastModeOff') = 'true' AND (json_type(launch_options,'$.fastMode') IS NULL OR json_type(launch_options,'$.fastMode') = 'false'))
    ELSE 0 END;

CREATE TABLE session_agent_settings_operations (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    request_json TEXT NOT NULL CHECK (json_valid(request_json)),
    phase TEXT NOT NULL CHECK (phase IN ('pending','applied','cancelled','failed','recovery_required')),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    UNIQUE(session_id,idempotency_key)
);
CREATE UNIQUE INDEX session_agent_settings_one_pending ON session_agent_settings_operations(session_id) WHERE phase IN ('pending','recovery_required');
CREATE TABLE session_agent_settings_audit (
    operation_id TEXT NOT NULL REFERENCES session_agent_settings_operations(id) ON DELETE CASCADE,
    phase TEXT NOT NULL CHECK (phase IN ('pending','applied','cancelled','failed','recovery_required')),
    recorded_at TIMESTAMP NOT NULL,
    PRIMARY KEY(operation_id,phase)
);

-- +goose StatementBegin
CREATE TRIGGER sessions_agent_settings_revision
AFTER UPDATE OF model,effort,agent_fast_mode ON sessions
WHEN NEW.agent_settings_revision = OLD.agent_settings_revision
 AND (NEW.model IS NOT OLD.model OR NEW.effort IS NOT OLD.effort OR NEW.agent_fast_mode IS NOT OLD.agent_fast_mode)
BEGIN
 UPDATE sessions SET agent_settings_revision = OLD.agent_settings_revision + 1 WHERE id = NEW.id;
END;
CREATE TRIGGER sessions_agent_settings_legacy_launch
AFTER UPDATE OF launch_options ON sessions
WHEN OLD.launch_options IS NOT NEW.launch_options
BEGIN
 UPDATE sessions SET agent_fast_mode = CASE
  WHEN json_valid(NEW.launch_options) THEN CASE
   WHEN json_type(NEW.launch_options,'$.fastMode') = 'true' AND (json_type(NEW.launch_options,'$.fastModeOff') IS NULL OR json_type(NEW.launch_options,'$.fastModeOff') = 'false') THEN 'on'
   WHEN json_type(NEW.launch_options,'$.fastModeOff') = 'true' AND (json_type(NEW.launch_options,'$.fastMode') IS NULL OR json_type(NEW.launch_options,'$.fastMode') = 'false') THEN 'off'
   ELSE 'default' END
  ELSE 'default' END
 WHERE id = NEW.id;
END;
CREATE TRIGGER sessions_agent_settings_cdc
AFTER UPDATE OF agent_settings_revision ON sessions
WHEN NEW.agent_settings_revision <> OLD.agent_settings_revision
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 VALUES(NEW.project_id,NEW.id,'session_updated',json_object('id',NEW.id),CURRENT_TIMESTAMP);
END;
CREATE TRIGGER session_agent_settings_operations_cdc
AFTER UPDATE OF phase ON session_agent_settings_operations
WHEN NEW.phase <> OLD.phase
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,id,'session_updated',json_object('id',id),CURRENT_TIMESTAMP FROM sessions WHERE id=NEW.session_id;
END;
CREATE TRIGGER session_agent_settings_operations_insert_cdc
AFTER INSERT ON session_agent_settings_operations
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,id,'session_updated',json_object('id',id),CURRENT_TIMESTAMP FROM sessions WHERE id=NEW.session_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER session_agent_settings_operations_insert_cdc;
DROP TRIGGER session_agent_settings_operations_cdc;
DROP TRIGGER sessions_agent_settings_cdc;
DROP TRIGGER sessions_agent_settings_legacy_launch;
DROP TRIGGER sessions_agent_settings_revision;
DROP TABLE session_agent_settings_audit;
DROP TABLE session_agent_settings_operations;
ALTER TABLE conversations DROP COLUMN agent_fast_mode;
ALTER TABLE sessions DROP COLUMN agent_settings_revision;
ALTER TABLE sessions DROP COLUMN agent_fast_mode;
