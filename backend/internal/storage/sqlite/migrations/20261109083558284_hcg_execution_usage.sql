-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;
BEGIN IMMEDIATE;
CREATE TEMP TABLE hcg_execution_sequence AS
 SELECT seq FROM sqlite_sequence WHERE name='model_usage_events';
-- Provenance only: all token and cost facts remain in model_usage_events.
CREATE TABLE usage_execution_sources (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 billing_provider TEXT NOT NULL CHECK(billing_provider IN ('openai','anthropic')),
 profile_id TEXT NOT NULL,
 profile_revision TEXT NOT NULL,
 profile_digest TEXT NOT NULL,
 delegation_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL,
 request_id TEXT NOT NULL,
 receipt_digest TEXT NOT NULL,
 identity_json TEXT NOT NULL CHECK(json_valid(identity_json)),
 UNIQUE(session_id,billing_provider,profile_id,profile_revision,delegation_id,attempt_id)
);
CREATE TABLE model_usage_events_next (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    binding_id                  INTEGER REFERENCES usage_bindings (id) ON DELETE CASCADE,
    usage_source_id             INTEGER REFERENCES usage_sources (id) ON DELETE CASCADE,
    execution_source_id INTEGER UNIQUE REFERENCES usage_execution_sources(id) ON DELETE CASCADE,
    provider_id                 TEXT NOT NULL CHECK (provider_id IN ('openai', 'anthropic')),
    billing_provider_id         TEXT CHECK (billing_provider_id IS NULL OR trim(billing_provider_id) <> ''),
    model_id                    TEXT NOT NULL CHECK (trim(model_id) <> ''),
    usage_measurement_kind      TEXT NOT NULL
        CHECK (usage_measurement_kind IN ('native_reported', 'provider_reported', 'ao_estimated', 'mixed', 'unknown')),
    input_tokens                INTEGER CHECK (input_tokens IS NULL OR input_tokens >= 0),
    cached_input_tokens         INTEGER CHECK (cached_input_tokens IS NULL OR cached_input_tokens >= 0),
    uncached_input_tokens       INTEGER CHECK (uncached_input_tokens IS NULL OR uncached_input_tokens >= 0),
    output_tokens               INTEGER CHECK (output_tokens IS NULL OR output_tokens >= 0),
    provider_usage_json         TEXT
        CHECK (provider_usage_json IS NULL
               OR (json_valid(provider_usage_json) AND json_type(provider_usage_json, '$') = 'object')),
    source_event_key            TEXT NOT NULL CHECK (trim(source_event_key) <> ''),
    created_at                  TIMESTAMP,
    input_cost_nanos            INTEGER CHECK (input_cost_nanos IS NULL OR input_cost_nanos >= 0),
    cached_input_cost_nanos     INTEGER CHECK (cached_input_cost_nanos IS NULL OR cached_input_cost_nanos >= 0),
    output_cost_nanos           INTEGER CHECK (output_cost_nanos IS NULL OR output_cost_nanos >= 0),
    estimated_cost_nanos        INTEGER CHECK (estimated_cost_nanos IS NULL OR estimated_cost_nanos >= 0),
    pricing_version             TEXT NOT NULL DEFAULT '',
    billing_provider_source TEXT CHECK (billing_provider_source IS NULL OR billing_provider_source IN ('observed', 'inferred')),
    UNIQUE (binding_id, source_event_key),
    CHECK ((execution_source_id IS NULL AND binding_id IS NOT NULL AND usage_source_id IS NOT NULL)
        OR (execution_source_id IS NOT NULL AND binding_id IS NULL AND usage_source_id IS NULL)),
    CHECK (input_tokens IS NULL OR cached_input_tokens IS NULL OR uncached_input_tokens IS NULL
           OR input_tokens = cached_input_tokens + uncached_input_tokens)
);

INSERT INTO model_usage_events_next (id,binding_id,usage_source_id,provider_id,billing_provider_id,model_id,usage_measurement_kind,input_tokens,cached_input_tokens,uncached_input_tokens,output_tokens,provider_usage_json,source_event_key,created_at,input_cost_nanos,cached_input_cost_nanos,output_cost_nanos,estimated_cost_nanos,pricing_version,billing_provider_source) SELECT id,binding_id,usage_source_id,provider_id,billing_provider_id,model_id,usage_measurement_kind,input_tokens,cached_input_tokens,uncached_input_tokens,output_tokens,provider_usage_json,source_event_key,created_at,input_cost_nanos,cached_input_cost_nanos,output_cost_nanos,estimated_cost_nanos,pricing_version,billing_provider_source FROM model_usage_events;
DROP TABLE model_usage_events;
ALTER TABLE model_usage_events_next RENAME TO model_usage_events;
INSERT INTO sqlite_sequence(name,seq)
 SELECT 'model_usage_events',COALESCE((SELECT MAX(seq) FROM hcg_execution_sequence),0)
 WHERE NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='model_usage_events');
UPDATE sqlite_sequence SET seq=MAX(seq,COALESCE((SELECT MAX(seq) FROM hcg_execution_sequence),0))
 WHERE name='model_usage_events';
DROP TABLE hcg_execution_sequence;

CREATE INDEX idx_model_usage_events_binding_model ON model_usage_events (binding_id, model_id);
CREATE INDEX idx_model_usage_events_usage_source ON model_usage_events (usage_source_id);
CREATE INDEX idx_model_usage_events_cost_candidates
    ON model_usage_events (billing_provider_id, pricing_version, id)
    WHERE estimated_cost_nanos IS NULL;
CREATE INDEX idx_model_usage_events_canonical_cost_candidates
    ON model_usage_events (
        CASE lower(trim(billing_provider_id))
            WHEN 'z.ai' THEN 'zai'
            ELSE lower(trim(billing_provider_id))
        END,
        id
    )
    WHERE billing_provider_id IS NOT NULL
      AND estimated_cost_nanos IS NULL;

CREATE INDEX idx_model_usage_events_open_attribution ON model_usage_events(usage_source_id,id)
 WHERE billing_provider_id IS NULL OR billing_provider_source = 'inferred';

CREATE TRIGGER execution_usage_cdc_insert AFTER INSERT ON model_usage_events
WHEN NEW.execution_source_id IS NOT NULL
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,session_id,'session_updated',json_object('id',session_id),NEW.created_at
 FROM usage_execution_sources WHERE id=NEW.execution_source_id;
END;
COMMIT;
PRAGMA foreign_keys=ON;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- A native-only downgrade cannot represent execution receipts. Refuse before
-- altering durable state; never silently delete the authoritative evidence.
CREATE TEMP TABLE IF NOT EXISTS hcg_execution_down_guard (count INTEGER CHECK(count=0));
INSERT INTO hcg_execution_down_guard SELECT COUNT(*) FROM usage_execution_sources;
DROP TABLE hcg_execution_down_guard;
PRAGMA foreign_keys=OFF;
BEGIN IMMEDIATE;
CREATE TEMP TABLE hcg_execution_sequence AS
 SELECT seq FROM sqlite_sequence WHERE name='model_usage_events';
CREATE TABLE model_usage_events_native (
    id                          INTEGER PRIMARY KEY AUTOINCREMENT,
    binding_id                  INTEGER NOT NULL REFERENCES usage_bindings (id) ON DELETE CASCADE,
    usage_source_id             INTEGER NOT NULL REFERENCES usage_sources (id) ON DELETE CASCADE,
    provider_id                 TEXT NOT NULL CHECK (provider_id IN ('openai', 'anthropic')),
    billing_provider_id         TEXT CHECK (billing_provider_id IS NULL OR trim(billing_provider_id) <> ''),
    model_id                    TEXT NOT NULL CHECK (trim(model_id) <> ''),
    usage_measurement_kind      TEXT NOT NULL
        CHECK (usage_measurement_kind IN ('native_reported', 'ao_estimated', 'mixed', 'unknown')),
    input_tokens                INTEGER CHECK (input_tokens IS NULL OR input_tokens >= 0),
    cached_input_tokens         INTEGER CHECK (cached_input_tokens IS NULL OR cached_input_tokens >= 0),
    uncached_input_tokens       INTEGER CHECK (uncached_input_tokens IS NULL OR uncached_input_tokens >= 0),
    output_tokens               INTEGER CHECK (output_tokens IS NULL OR output_tokens >= 0),
    provider_usage_json         TEXT
        CHECK (provider_usage_json IS NULL
               OR (json_valid(provider_usage_json) AND json_type(provider_usage_json, '$') = 'object')),
    source_event_key            TEXT NOT NULL CHECK (trim(source_event_key) <> ''),
    created_at                  TIMESTAMP,
    input_cost_nanos            INTEGER CHECK (input_cost_nanos IS NULL OR input_cost_nanos >= 0),
    cached_input_cost_nanos     INTEGER CHECK (cached_input_cost_nanos IS NULL OR cached_input_cost_nanos >= 0),
    output_cost_nanos           INTEGER CHECK (output_cost_nanos IS NULL OR output_cost_nanos >= 0),
    estimated_cost_nanos        INTEGER CHECK (estimated_cost_nanos IS NULL OR estimated_cost_nanos >= 0),
    pricing_version             TEXT NOT NULL DEFAULT '',
    billing_provider_source TEXT CHECK (billing_provider_source IS NULL OR billing_provider_source IN ('observed', 'inferred')),
    UNIQUE (binding_id, source_event_key),
    CHECK (input_tokens IS NULL OR cached_input_tokens IS NULL OR uncached_input_tokens IS NULL
           OR input_tokens = cached_input_tokens + uncached_input_tokens)
);
INSERT INTO model_usage_events_native (id,binding_id,usage_source_id,provider_id,billing_provider_id,model_id,usage_measurement_kind,input_tokens,cached_input_tokens,uncached_input_tokens,output_tokens,provider_usage_json,source_event_key,created_at,input_cost_nanos,cached_input_cost_nanos,output_cost_nanos,estimated_cost_nanos,pricing_version,billing_provider_source) SELECT id,binding_id,usage_source_id,provider_id,billing_provider_id,model_id,usage_measurement_kind,input_tokens,cached_input_tokens,uncached_input_tokens,output_tokens,provider_usage_json,source_event_key,created_at,input_cost_nanos,cached_input_cost_nanos,output_cost_nanos,estimated_cost_nanos,pricing_version,billing_provider_source FROM model_usage_events;
DROP TABLE model_usage_events;
ALTER TABLE model_usage_events_native RENAME TO model_usage_events;
INSERT INTO sqlite_sequence(name,seq)
 SELECT 'model_usage_events',COALESCE((SELECT MAX(seq) FROM hcg_execution_sequence),0)
 WHERE NOT EXISTS(SELECT 1 FROM sqlite_sequence WHERE name='model_usage_events');
UPDATE sqlite_sequence SET seq=MAX(seq,COALESCE((SELECT MAX(seq) FROM hcg_execution_sequence),0))
 WHERE name='model_usage_events';
DROP TABLE hcg_execution_sequence;
CREATE INDEX idx_model_usage_events_binding_model ON model_usage_events (binding_id, model_id);
CREATE INDEX idx_model_usage_events_usage_source ON model_usage_events (usage_source_id);
CREATE INDEX idx_model_usage_events_cost_candidates
    ON model_usage_events (billing_provider_id, pricing_version, id)
    WHERE estimated_cost_nanos IS NULL;
CREATE INDEX idx_model_usage_events_canonical_cost_candidates
    ON model_usage_events (
        CASE lower(trim(billing_provider_id))
            WHEN 'z.ai' THEN 'zai'
            ELSE lower(trim(billing_provider_id))
        END,
        id
    )
    WHERE billing_provider_id IS NOT NULL
      AND estimated_cost_nanos IS NULL;

CREATE INDEX idx_model_usage_events_open_attribution ON model_usage_events(usage_source_id,id)
 WHERE billing_provider_id IS NULL OR billing_provider_source = 'inferred';

DROP TABLE usage_execution_sources;
COMMIT;
PRAGMA foreign_keys=ON;
-- +goose StatementEnd
