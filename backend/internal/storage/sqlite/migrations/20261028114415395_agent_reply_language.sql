-- +goose Up
ALTER TABLE app_settings ADD COLUMN reply_language TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE app_settings DROP COLUMN reply_language;
