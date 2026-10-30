-- +goose Up
CREATE TABLE reviewer_agent_settings (
    review_id TEXT PRIMARY KEY REFERENCES review(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    settings_json TEXT NOT NULL CHECK (json_valid(settings_json) AND json_type(settings_json) = 'object'),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE reviewer_agent_settings_operations (
    id TEXT PRIMARY KEY,
    review_id TEXT NOT NULL REFERENCES review(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL CHECK (revision > 0),
    phase TEXT NOT NULL DEFAULT 'pinned' CHECK (phase = 'pinned'),
    apply_mode TEXT NOT NULL DEFAULT 'next-pass' CHECK (apply_mode = 'next-pass'),
    idempotency_key TEXT NOT NULL,
    request_fingerprint TEXT NOT NULL,
    operation_json TEXT NOT NULL CHECK (json_valid(operation_json) AND json_type(operation_json) = 'object'),
    created_at TIMESTAMP NOT NULL,
    UNIQUE(review_id, idempotency_key),
    UNIQUE(review_id, revision)
);

-- NULL means an older pass has no recorded selection. Never backfill it from
-- the worker's current settings or the project's current reviewer defaults.
ALTER TABLE review_run ADD COLUMN agent_settings_json TEXT
    CHECK (agent_settings_json IS NULL OR (json_valid(agent_settings_json) AND json_type(agent_settings_json) = 'object'));
ALTER TABLE review_run ADD COLUMN agent_settings_revision INTEGER
    CHECK (agent_settings_revision IS NULL OR agent_settings_revision >= 0);

-- +goose StatementBegin
CREATE TRIGGER reviewer_agent_settings_cdc_insert AFTER INSERT ON reviewer_agent_settings BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT project_id, session_id, 'session_updated', json_object('id', session_id, 'reviewId', NEW.review_id, 'reviewSettingsRevision', NEW.revision), NEW.updated_at
    FROM review WHERE id = NEW.review_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER reviewer_agent_settings_cdc_update AFTER UPDATE ON reviewer_agent_settings BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT project_id, session_id, 'session_updated', json_object('id', session_id, 'reviewId', NEW.review_id, 'reviewSettingsRevision', NEW.revision), NEW.updated_at
    FROM review WHERE id = NEW.review_id;
END;
-- +goose StatementEnd

-- +goose Down
-- Reverting the entry points must not erase saved preferences or pass facts.
CREATE TEMP TABLE reviewer_settings_downgrade_guard (ok INTEGER CHECK (ok=1));
INSERT INTO reviewer_settings_downgrade_guard SELECT 0 FROM reviewer_agent_settings LIMIT 1;
INSERT INTO reviewer_settings_downgrade_guard SELECT 0 FROM review_run WHERE agent_settings_json IS NOT NULL LIMIT 1;
DROP TABLE reviewer_settings_downgrade_guard;
DROP TRIGGER reviewer_agent_settings_cdc_update;
DROP TRIGGER reviewer_agent_settings_cdc_insert;
ALTER TABLE review_run DROP COLUMN agent_settings_revision;
ALTER TABLE review_run DROP COLUMN agent_settings_json;
DROP TABLE reviewer_agent_settings_operations;
DROP TABLE reviewer_agent_settings;
