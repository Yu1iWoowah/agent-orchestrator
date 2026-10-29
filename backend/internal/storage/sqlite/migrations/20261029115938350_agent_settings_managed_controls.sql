-- +goose Up
ALTER TABLE sessions ADD COLUMN agent_settings_launch_id TEXT NOT NULL DEFAULT '';
CREATE TABLE agent_settings_authority_keys (
    singleton INTEGER PRIMARY KEY CHECK (singleton=1),
    key_bytes BLOB NOT NULL CHECK (typeof(key_bytes)='blob' AND length(key_bytes)=32)
);
ALTER TABLE session_agent_settings_operations ADD COLUMN error_code TEXT NOT NULL DEFAULT ''
CHECK (length(error_code)<=96 AND error_code NOT GLOB '*[^A-Z0-9_]*');
ALTER TABLE session_agent_settings_audit ADD COLUMN error_code TEXT NOT NULL DEFAULT ''
CHECK (length(error_code)<=96 AND error_code NOT GLOB '*[^A-Z0-9_]*');

-- +goose Down
ALTER TABLE sessions DROP COLUMN agent_settings_launch_id;
ALTER TABLE session_agent_settings_audit DROP COLUMN error_code;
ALTER TABLE session_agent_settings_operations DROP COLUMN error_code;
DROP TABLE agent_settings_authority_keys;
