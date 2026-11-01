-- +goose Up
CREATE TABLE guardian_reviews (
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
CREATE INDEX guardian_reviews_retention ON guardian_reviews(retention_at);
-- +goose StatementBegin
CREATE TRIGGER guardian_reviews_cdc_insert AFTER INSERT ON guardian_reviews BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=NEW.epoch_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_reviews_cdc_update AFTER UPDATE ON guardian_reviews BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=NEW.epoch_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER guardian_reviews_cdc_delete AFTER DELETE ON guardian_reviews BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id),CURRENT_TIMESTAMP
 FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id WHERE e.id=OLD.epoch_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TABLE guardian_reviews;
