-- +goose Up
CREATE TABLE conversation_plan_heads (
 conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
 branch_id TEXT NOT NULL REFERENCES conversation_branches(id) ON DELETE CASCADE,
 source_activity_id TEXT NOT NULL,
 turn_id TEXT NOT NULL DEFAULT '',
 accepted_sequence INTEGER NOT NULL,
 revision INTEGER NOT NULL,
 plan_json TEXT NOT NULL CHECK(json_valid(plan_json)),
 updated_at DATETIME NOT NULL,
 PRIMARY KEY(conversation_id, branch_id)
);
CREATE TABLE conversation_plan_versions (
 conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
 branch_id TEXT NOT NULL REFERENCES conversation_branches(id) ON DELETE CASCADE,
 source_activity_id TEXT NOT NULL,
 turn_id TEXT NOT NULL DEFAULT '',
 accepted_sequence INTEGER NOT NULL,
 revision INTEGER NOT NULL,
 plan_json TEXT NOT NULL CHECK(json_valid(plan_json)),
 updated_at DATETIME NOT NULL,
 PRIMARY KEY(conversation_id, branch_id, accepted_sequence)
);
CREATE INDEX conversation_plan_versions_source ON conversation_plan_versions(conversation_id, source_activity_id, accepted_sequence DESC);
-- +goose StatementBegin
CREATE TRIGGER conversation_plan_heads_cdc_update AFTER UPDATE ON conversation_plan_heads
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('sessionId',s.id,'conversationId',NEW.conversation_id),NEW.updated_at
 FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER conversation_plan_heads_cdc_insert AFTER INSERT ON conversation_plan_heads
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('sessionId',s.id,'conversationId',NEW.conversation_id),NEW.updated_at
 FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
-- +goose Down
DROP TRIGGER conversation_plan_heads_cdc_update;
DROP TRIGGER conversation_plan_heads_cdc_insert;
DROP TABLE conversation_plan_heads;
DROP TABLE conversation_plan_versions;
