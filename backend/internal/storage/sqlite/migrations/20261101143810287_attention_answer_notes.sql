-- +goose Up
ALTER TABLE attention_answers ADD COLUMN notes TEXT;

-- +goose Down
ALTER TABLE attention_answers DROP COLUMN notes;
