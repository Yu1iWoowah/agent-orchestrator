-- +goose Up
ALTER TABLE session_agent_settings_operations ADD COLUMN execution_json TEXT NOT NULL DEFAULT ''
CHECK (execution_json='' OR json_valid(execution_json));

-- +goose Down
ALTER TABLE session_agent_settings_operations DROP COLUMN execution_json;
