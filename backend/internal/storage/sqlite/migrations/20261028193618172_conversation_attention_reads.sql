-- +goose Up
-- Seen revisions are separate from the provider's durable pending/resolved facts.
-- The single local operator scope is shared by the desktop and paired clients.
CREATE TABLE attention_reads (
 operator_scope TEXT NOT NULL DEFAULT 'local' CHECK(operator_scope = 'local'),
 conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
 activity_id TEXT NOT NULL REFERENCES conversation_activities(id) ON DELETE CASCADE,
 last_seen_revision INTEGER NOT NULL CHECK(last_seen_revision >= 0),
 seen_at DATETIME NOT NULL,
 PRIMARY KEY(operator_scope, conversation_id, activity_id)
);
CREATE INDEX conversation_activities_attention ON conversation_activities(conversation_id,kind,branch_id,sequence DESC);
-- +goose StatementBegin
CREATE TRIGGER attention_reads_cdc_insert AFTER INSERT ON attention_reads
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('sessionId',s.id,'conversationId',NEW.conversation_id),NEW.seen_at
 FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_reads_cdc_update AFTER UPDATE ON attention_reads
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('sessionId',s.id,'conversationId',NEW.conversation_id),NEW.seen_at
 FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
-- +goose Down
DROP TRIGGER attention_reads_cdc_update;
DROP TRIGGER attention_reads_cdc_insert;
DROP TABLE attention_reads;
DROP INDEX conversation_activities_attention;
