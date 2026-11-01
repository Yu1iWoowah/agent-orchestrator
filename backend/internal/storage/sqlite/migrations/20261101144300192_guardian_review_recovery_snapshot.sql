-- +goose Up
CREATE TABLE guardian_review_pending (
 epoch_id INTEGER NOT NULL REFERENCES guardian_epochs(id) ON DELETE CASCADE,
 parent_thread_id TEXT NOT NULL CHECK(length(parent_thread_id) BETWEEN 1 AND 256),
 review_id TEXT NOT NULL CHECK(length(review_id) BETWEEN 1 AND 256),
 parent_turn_id TEXT NOT NULL CHECK(length(parent_turn_id) BETWEEN 1 AND 256),
 target_item_id TEXT CHECK(length(target_item_id) BETWEEN 1 AND 256),
 action TEXT NOT NULL CHECK(action IN ('command','execve','writeStdin','applyPatch','networkAccess','mcpToolCall','requestPermissions','unknown')),
 status TEXT NOT NULL CHECK(status IN ('inProgress','approved','denied','timedOut','aborted','unknown')),
 decision_source TEXT NOT NULL CHECK(decision_source IN ('','agent','unknown')),
 started_at TIMESTAMP,
 completed_at TIMESTAMP,
 first_seen_at TIMESTAMP NOT NULL,
 retention_at TIMESTAMP NOT NULL,
 saw_started INTEGER NOT NULL CHECK(saw_started IN (0,1)),
 saw_completed INTEGER NOT NULL CHECK(saw_completed IN (0,1)),
 conflict INTEGER NOT NULL DEFAULT 0 CHECK(conflict IN (0,1)),
 PRIMARY KEY(epoch_id,parent_thread_id,review_id)
);
CREATE INDEX guardian_review_pending_retention ON guardian_review_pending(retention_at);

CREATE TABLE guardian_ingestion_state (
 id INTEGER PRIMARY KEY CHECK(id=1),
 sequence INTEGER NOT NULL CHECK(typeof(sequence)='integer' AND sequence>=0)
);
INSERT INTO guardian_ingestion_state VALUES(1,0);
ALTER TABLE guardian_requests ADD COLUMN ingestion_sequence INTEGER NOT NULL DEFAULT 0;
ALTER TABLE guardian_reviews ADD COLUMN ingestion_sequence INTEGER NOT NULL DEFAULT 0;
DROP TRIGGER guardian_reviews_cdc_update;
-- +goose StatementBegin
CREATE TRIGGER guardian_reviews_cdc_update AFTER UPDATE ON guardian_reviews
WHEN OLD.parent_turn_id IS NOT NEW.parent_turn_id OR OLD.target_item_id IS NOT NEW.target_item_id OR OLD.action IS NOT NEW.action OR OLD.status IS NOT NEW.status OR OLD.decision_source IS NOT NEW.decision_source OR OLD.started_at IS NOT NEW.started_at OR OLD.completed_at IS NOT NEW.completed_at OR OLD.first_seen_at IS NOT NEW.first_seen_at OR OLD.retention_at IS NOT NEW.retention_at OR OLD.saw_started IS NOT NEW.saw_started OR OLD.saw_completed IS NOT NEW.saw_completed OR OLD.conflict IS NOT NEW.conflict BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=NEW.epoch_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_requests_ingestion AFTER INSERT ON guardian_requests BEGIN
 UPDATE guardian_ingestion_state SET sequence=sequence+1 WHERE id=1;
 UPDATE guardian_requests SET ingestion_sequence=(SELECT sequence FROM guardian_ingestion_state WHERE id=1) WHERE epoch_id=NEW.epoch_id AND thread_id=NEW.thread_id AND response_id=NEW.response_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_reviews_ingestion AFTER INSERT ON guardian_reviews BEGIN
 UPDATE guardian_ingestion_state SET sequence=sequence+1 WHERE id=1;
 UPDATE guardian_reviews SET ingestion_sequence=(SELECT sequence FROM guardian_ingestion_state WHERE id=1) WHERE epoch_id=NEW.epoch_id AND parent_thread_id=NEW.parent_thread_id AND review_id=NEW.review_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER guardian_requests_ingestion;
DROP TRIGGER guardian_reviews_ingestion;
DROP TRIGGER guardian_reviews_cdc_update;
-- +goose StatementBegin
CREATE TRIGGER guardian_reviews_cdc_update AFTER UPDATE ON guardian_reviews BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=NEW.epoch_id;
END;
-- +goose StatementEnd
ALTER TABLE guardian_requests DROP COLUMN ingestion_sequence;
ALTER TABLE guardian_reviews DROP COLUMN ingestion_sequence;
DROP TABLE guardian_ingestion_state;
DROP TABLE guardian_review_pending;
