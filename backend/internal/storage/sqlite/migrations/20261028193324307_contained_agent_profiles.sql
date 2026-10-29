-- +goose Up
-- Existing profiles keep their device-linked preparation. New profiles use
-- only AO-owned history; no credentials or history are migrated implicitly.
ALTER TABLE agent_accounts ADD COLUMN profile_kind TEXT NOT NULL DEFAULT 'external-linked'
CHECK (profile_kind IN ('external-linked', 'contained-v1'));

-- +goose Down
ALTER TABLE agent_accounts DROP COLUMN profile_kind;
