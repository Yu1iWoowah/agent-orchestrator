-- +goose Up
-- A fresh SQL replay loses these upstream columns in the fork table rebuilds.
-- The runner validates and adopts already-present columns on u3/F1 upgrades.
ALTER TABLE conversation_turns ADD COLUMN failure_code TEXT NOT NULL DEFAULT '';
ALTER TABLE model_usage_events ADD COLUMN service_tier TEXT
    CHECK (service_tier IS NULL OR (trim(service_tier) <> '' AND length(service_tier) <= 64));
ALTER TABLE model_usage_events ADD COLUMN provider_message_id TEXT
    CHECK (provider_message_id IS NULL OR (trim(provider_message_id) <> '' AND length(provider_message_id) <= 256));

DROP INDEX IF EXISTS idx_conversation_turns_failed_recent;
CREATE INDEX idx_conversation_turns_failed_recent
    ON conversation_turns(completed_at) WHERE state = 'failed';
DROP INDEX IF EXISTS idx_model_usage_events_provider_message;
CREATE UNIQUE INDEX idx_model_usage_events_provider_message
    ON model_usage_events(provider_id, provider_message_id) WHERE provider_message_id IS NOT NULL;
DROP INDEX IF EXISTS idx_model_usage_events_priced_version;
CREATE INDEX idx_model_usage_events_priced_version
    ON model_usage_events(pricing_version, id) WHERE estimated_cost_nanos IS NOT NULL;
DROP INDEX IF EXISTS idx_model_usage_events_model;
CREATE INDEX idx_model_usage_events_model ON model_usage_events(model_id, id);

-- Keep the fork lineage names and give upstream's delete access a distinct name.
DROP INDEX IF EXISTS idx_conversation_messages_turn;
CREATE INDEX idx_conversation_messages_turn ON conversation_messages(conversation_id, turn_id, sequence);
DROP INDEX IF EXISTS idx_conversation_activities_turn;
CREATE INDEX idx_conversation_activities_turn ON conversation_activities(conversation_id, turn_id, sequence);
DROP INDEX IF EXISTS idx_conversation_messages_turn_delete;
CREATE INDEX idx_conversation_messages_turn_delete ON conversation_messages(turn_id) WHERE turn_id IS NOT NULL;
DROP INDEX IF EXISTS idx_conversation_activities_turn_delete;
CREATE INDEX idx_conversation_activities_turn_delete ON conversation_activities(turn_id) WHERE turn_id IS NOT NULL;

-- +goose Down
-- The two input histories had different columns and index definitions.
-- A downgrade must restore its pre-upgrade backup instead of guessing one.
CREATE TEMP TABLE upstream_sync_down_guard (ok INTEGER CHECK(ok = 1));
INSERT INTO upstream_sync_down_guard VALUES(0);
DROP TABLE upstream_sync_down_guard;
