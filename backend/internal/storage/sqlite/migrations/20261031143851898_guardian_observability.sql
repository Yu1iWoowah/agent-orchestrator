-- +goose Up
-- All new observation state is metadata-only. Existing usage tables are untouched.
CREATE TABLE guardian_project_policy (
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 enabled_at TIMESTAMP NOT NULL
);
INSERT INTO guardian_project_policy SELECT id, COALESCE(json_extract(config,'$.guardianObservability.enabled') = 1,0), strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM projects;
-- +goose StatementBegin
CREATE TRIGGER guardian_project_insert AFTER INSERT ON projects BEGIN
 INSERT INTO guardian_project_policy VALUES(NEW.id, COALESCE(json_extract(NEW.config,'$.guardianObservability.enabled') = 1,0), strftime('%Y-%m-%dT%H:%M:%fZ','now'));
END;
CREATE TRIGGER guardian_project_config AFTER UPDATE OF config ON projects BEGIN
 UPDATE guardian_project_policy SET
 enabled_at = CASE WHEN enabled = 0 AND COALESCE(json_extract(NEW.config,'$.guardianObservability.enabled') = 1,0) THEN strftime('%Y-%m-%dT%H:%M:%fZ','now') ELSE enabled_at END,
 enabled = COALESCE(json_extract(NEW.config,'$.guardianObservability.enabled') = 1,0)
 WHERE project_id = NEW.id;
END;
-- +goose StatementEnd
CREATE TABLE guardian_watermarks (
 session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
 cleaned_before TIMESTAMP NOT NULL
);
CREATE TABLE guardian_epochs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 binding_id INTEGER NOT NULL REFERENCES usage_bindings(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 account_id TEXT NOT NULL CHECK(length(account_id) BETWEEN 1 AND 256),
 account_bound_at TIMESTAMP NOT NULL,
 first_seen_at TIMESTAMP NOT NULL,
 UNIQUE(binding_id,account_id,account_bound_at)
);
CREATE TABLE guardian_threads (
 epoch_id INTEGER NOT NULL REFERENCES guardian_epochs(id) ON DELETE CASCADE,
 thread_id TEXT NOT NULL CHECK(length(thread_id) BETWEEN 1 AND 256),
 parent_id TEXT NOT NULL CHECK(length(parent_id) BETWEEN 1 AND 256),
 cli_version TEXT NOT NULL CHECK(length(cli_version)<=64),
	 retention_at TIMESTAMP NOT NULL,
 first_seen_at TIMESTAMP NOT NULL,
 last_seen_at TIMESTAMP NOT NULL,
 PRIMARY KEY(epoch_id,thread_id)
);
-- Minimal replay state survives deletion of expired native IDs. Kernel file
-- identity is neither a path nor a fingerprint of provider content.
CREATE TABLE guardian_positions (
 epoch_id INTEGER NOT NULL REFERENCES guardian_epochs(id) ON DELETE CASCADE,
 file_identity TEXT NOT NULL CHECK(length(file_identity) BETWEEN 1 AND 128),
 generation INTEGER NOT NULL CHECK(generation>=0),
 byte_offset INTEGER NOT NULL CHECK(byte_offset>=0),
 read_start_offset INTEGER NOT NULL DEFAULT 0 CHECK(read_start_offset>=0),
 replay_until INTEGER NOT NULL DEFAULT 0 CHECK(replay_until>=0),
 source_size INTEGER NOT NULL DEFAULT 0 CHECK(source_size>=0),
 source_modified_at TIMESTAMP,
 discarding INTEGER NOT NULL CHECK(discarding IN (0,1)),
 updated_at TIMESTAMP NOT NULL,
 PRIMARY KEY(epoch_id,file_identity)
);
CREATE TABLE guardian_cursors (
 epoch_id INTEGER NOT NULL,
 thread_id TEXT NOT NULL,
 generation INTEGER NOT NULL CHECK(generation>=0),
 byte_offset INTEGER NOT NULL CHECK(byte_offset>=0),
 read_start_offset INTEGER NOT NULL DEFAULT 0 CHECK(read_start_offset>=0),
 replay_until INTEGER NOT NULL DEFAULT 0 CHECK(replay_until>=0),
 source_size INTEGER NOT NULL DEFAULT 0 CHECK(source_size>=0),
 source_modified_at TIMESTAMP,
 file_identity TEXT NOT NULL CHECK(length(file_identity)<=128),
	 discarding INTEGER NOT NULL DEFAULT 0 CHECK(discarding IN (0,1)),
 updated_at TIMESTAMP NOT NULL,
 PRIMARY KEY(epoch_id,thread_id),
 FOREIGN KEY(epoch_id,thread_id) REFERENCES guardian_threads(epoch_id,thread_id) ON DELETE CASCADE
);
CREATE TABLE guardian_turn_models (
 epoch_id INTEGER NOT NULL,
 thread_id TEXT NOT NULL,
 turn_id TEXT NOT NULL CHECK(length(turn_id) BETWEEN 1 AND 256),
 model_id TEXT NOT NULL CHECK(length(model_id) BETWEEN 1 AND 128),
 provider_at TIMESTAMP NOT NULL,
 conflict INTEGER NOT NULL DEFAULT 0 CHECK(conflict IN (0,1)),
 PRIMARY KEY(epoch_id,thread_id,turn_id),
 FOREIGN KEY(epoch_id,thread_id) REFERENCES guardian_threads(epoch_id,thread_id) ON DELETE CASCADE
);
CREATE TABLE guardian_requests (
	 cli_version TEXT NOT NULL CHECK(length(cli_version) BETWEEN 1 AND 64),
 epoch_id INTEGER NOT NULL,
 thread_id TEXT NOT NULL,
 turn_id TEXT NOT NULL CHECK(length(turn_id) BETWEEN 1 AND 256),
 response_id TEXT NOT NULL CHECK(length(response_id) BETWEEN 1 AND 256),
 root_turn_id TEXT CHECK(root_turn_id IS NULL OR length(root_turn_id) BETWEEN 1 AND 256),
 model_id TEXT CHECK(model_id IS NULL OR length(model_id) BETWEEN 1 AND 128),
 input_tokens INTEGER CHECK(input_tokens>=0),
 cached_tokens INTEGER CHECK(cached_tokens>=0 AND (input_tokens IS NULL OR cached_tokens<=input_tokens)),
 cache_write_tokens INTEGER CHECK(cache_write_tokens>=0),
 output_tokens INTEGER CHECK(output_tokens>=0),
 reasoning_tokens INTEGER CHECK(reasoning_tokens>=0 AND (output_tokens IS NULL OR reasoning_tokens<=output_tokens)),
 total_tokens INTEGER CHECK(total_tokens>=0),
 provider_at TIMESTAMP NOT NULL,
 first_seen_at TIMESTAMP NOT NULL,
 source_generation INTEGER NOT NULL CHECK(source_generation>=0),
 source_offset INTEGER NOT NULL CHECK(source_offset>=0),
 conflict INTEGER NOT NULL DEFAULT 0 CHECK(conflict IN (0,1)),
 PRIMARY KEY(epoch_id,thread_id,response_id),
 FOREIGN KEY(epoch_id,thread_id) REFERENCES guardian_threads(epoch_id,thread_id) ON DELETE CASCADE
);
CREATE INDEX guardian_requests_retention ON guardian_requests(provider_at,first_seen_at);
CREATE INDEX guardian_requests_checkpoint ON guardian_requests(epoch_id,thread_id,source_generation,source_offset);
CREATE INDEX guardian_requests_turn ON guardian_requests(epoch_id,thread_id,turn_id);
-- Processing state uses only the existing kernel source identity and numeric
-- positions. No native thread IDs, paths, payloads, or content hashes survive.
CREATE TABLE guardian_anomaly_states (
 epoch_id INTEGER NOT NULL REFERENCES guardian_epochs(id) ON DELETE CASCADE,
 file_identity TEXT NOT NULL CHECK(length(file_identity)<=128),
 code TEXT NOT NULL CHECK(code IN ('invalid_metadata','skipped_unknown_time','owner_unresolved','owner_conflict','source_conflict','source_rotated','source_truncated','source_missing','partial_line','coverage_gap','unsupported_version')),
 source_size INTEGER NOT NULL CHECK(source_size>=0),
 source_modified_at TIMESTAMP,
 source_generation INTEGER NOT NULL CHECK(source_generation>=0),
 source_offset INTEGER NOT NULL CHECK(source_offset>=0),
 first_seen_at TIMESTAMP NOT NULL,
 PRIMARY KEY(epoch_id,file_identity,code)
);
CREATE TABLE guardian_anomalies (
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 code TEXT NOT NULL CHECK(code IN ('invalid_metadata','skipped_unknown_time','owner_unresolved','owner_conflict','source_conflict','source_rotated','source_truncated','source_missing','partial_line','coverage_gap','unsupported_version')),
 count INTEGER NOT NULL CHECK(count>=0),
 first_seen_at TIMESTAMP NOT NULL,
 PRIMARY KEY(session_id,code)
);
-- +goose StatementBegin
CREATE TRIGGER guardian_requests_cdc_insert AFTER INSERT ON guardian_requests BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=NEW.epoch_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_requests_cdc_update AFTER UPDATE ON guardian_requests
WHEN OLD.cli_version IS NOT NEW.cli_version
 OR OLD.turn_id IS NOT NEW.turn_id
 OR OLD.root_turn_id IS NOT NEW.root_turn_id
 OR OLD.model_id IS NOT NEW.model_id
 OR OLD.input_tokens IS NOT NEW.input_tokens
 OR OLD.cached_tokens IS NOT NEW.cached_tokens
 OR OLD.cache_write_tokens IS NOT NEW.cache_write_tokens
 OR OLD.output_tokens IS NOT NEW.output_tokens
 OR OLD.reasoning_tokens IS NOT NEW.reasoning_tokens
 OR OLD.total_tokens IS NOT NEW.total_tokens
 OR OLD.provider_at IS NOT NEW.provider_at
 OR OLD.first_seen_at IS NOT NEW.first_seen_at
 OR OLD.source_generation IS NOT NEW.source_generation
 OR OLD.source_offset IS NOT NEW.source_offset
 OR OLD.conflict IS NOT NEW.conflict
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=NEW.epoch_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_requests_cdc_delete AFTER DELETE ON guardian_requests BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=OLD.epoch_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_threads_cdc_insert AFTER INSERT ON guardian_threads BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=NEW.epoch_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_threads_cdc_update AFTER UPDATE ON guardian_threads
WHEN OLD.parent_id IS NOT NEW.parent_id
 OR OLD.cli_version IS NOT NEW.cli_version
 OR OLD.retention_at IS NOT NEW.retention_at
 OR OLD.first_seen_at IS NOT NEW.first_seen_at
 OR OLD.last_seen_at IS NOT NEW.last_seen_at
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=NEW.epoch_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_threads_cdc_delete AFTER DELETE ON guardian_threads BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=OLD.epoch_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_requests_conflict AFTER UPDATE OF conflict ON guardian_requests WHEN OLD.conflict=0 AND NEW.conflict=1 BEGIN
 INSERT INTO guardian_anomalies(session_id,code,count,first_seen_at)
 SELECT e.session_id,'source_conflict',1,strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM guardian_epochs e WHERE e.id=NEW.epoch_id
 ON CONFLICT(session_id,code) DO UPDATE SET count=MIN(count+1,9223372036854775807);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_turn_models_conflict AFTER UPDATE OF conflict ON guardian_turn_models WHEN OLD.conflict=0 AND NEW.conflict=1 BEGIN
 INSERT INTO guardian_anomalies(session_id,code,count,first_seen_at)
 SELECT e.session_id,'source_conflict',1,strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM guardian_epochs e WHERE e.id=NEW.epoch_id
 ON CONFLICT(session_id,code) DO UPDATE SET count=MIN(count+1,9223372036854775807);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER guardian_anomalies_cdc_insert AFTER INSERT ON guardian_anomalies BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM sessions s WHERE s.id=NEW.session_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_anomalies_cdc_update AFTER UPDATE ON guardian_anomalies BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM sessions s WHERE s.id=NEW.session_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_anomalies_cdc_delete AFTER DELETE ON guardian_anomalies BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM sessions s WHERE s.id=OLD.session_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_epochs_cdc_delete AFTER DELETE ON guardian_epochs BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM sessions s WHERE s.id=OLD.session_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER guardian_project_config;
DROP TRIGGER guardian_project_insert;
DROP TABLE guardian_anomaly_states;
DROP TABLE guardian_anomalies;
DROP TABLE guardian_requests;
DROP TABLE guardian_turn_models;
DROP TABLE guardian_cursors;
DROP TABLE guardian_threads;
DROP TABLE guardian_positions;
DROP TABLE guardian_epochs;
DROP TABLE guardian_watermarks;
DROP TABLE guardian_project_policy;
