-- +goose Up
ALTER TABLE sessions ADD COLUMN spawn_agent_settings_json TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN spawn_agent_settings_json;
