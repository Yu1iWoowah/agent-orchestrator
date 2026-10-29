-- +goose Up
-- A durable source generation avoids scanning historical sessions on every
-- notification tick. Ordinary token CDC never advances it; retention is safe.
CREATE TABLE project_attention_revisions (
 project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL
);
-- +goose StatementBegin
CREATE TRIGGER project_attention_revision_cdc AFTER INSERT ON change_log
WHEN NEW.project_id IS NOT NULL AND json_valid(NEW.payload)
 AND (json_extract(NEW.payload,'$.attentionChanged')=1 OR json_extract(NEW.payload,'$.questionId') IS NOT NULL) BEGIN
 INSERT INTO project_attention_revisions(project_id,revision) VALUES(NEW.project_id,1)
 ON CONFLICT(project_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_attention_question_deleted BEFORE DELETE ON attention_questions WHEN OLD.project_id IS NOT NULL BEGIN
 INSERT INTO project_attention_revisions(project_id,revision) VALUES(OLD.project_id,1)
 ON CONFLICT(project_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_attention_provider_deleted BEFORE DELETE ON conversation_activities
WHEN OLD.kind IN ('approval','user_input') OR OLD.provider_item_id LIKE 'ao-context-reset:%' BEGIN
 INSERT INTO project_attention_revisions(project_id,revision)
 SELECT s.project_id,1 FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=OLD.conversation_id AND s.project_id IS NOT NULL
 ON CONFLICT(project_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_attention_session_deleted BEFORE DELETE ON sessions WHEN OLD.project_id IS NOT NULL BEGIN
 INSERT INTO project_attention_revisions(project_id,revision) VALUES(OLD.project_id,1)
 ON CONFLICT(project_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER project_attention_conversation_deleted BEFORE DELETE ON conversations WHEN COALESCE(OLD.project_id,(SELECT project_id FROM sessions WHERE id=OLD.current_session_id)) IS NOT NULL BEGIN
 INSERT INTO project_attention_revisions(project_id,revision) VALUES(COALESCE(OLD.project_id,(SELECT project_id FROM sessions WHERE id=OLD.current_session_id)),1)
 ON CONFLICT(project_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
CREATE TABLE attention_report_questions (
 report_id TEXT PRIMARY KEY REFERENCES reports(id) ON DELETE CASCADE,
 question_id TEXT NOT NULL UNIQUE REFERENCES attention_questions(id) ON DELETE CASCADE,
 forwarded_by TEXT NOT NULL,
 created_at DATETIME NOT NULL
);
CREATE TABLE attention_notification_receipts (
 source_key TEXT PRIMARY KEY,
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE
);
CREATE TABLE attention_report_reads (
 report_id TEXT PRIMARY KEY REFERENCES reports(id) ON DELETE CASCADE,
 seen_at DATETIME NOT NULL
);
-- +goose StatementBegin
CREATE TRIGGER attention_report_reads_cdc AFTER INSERT ON attention_report_reads BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,session_id,'session_updated',json_object('sessionId',session_id,'reportId',NEW.report_id),NEW.seen_at FROM reports WHERE id=NEW.report_id;
END;
-- +goose StatementEnd

-- Enrich existing CDC events, without an extra activity event per provider update.
DROP TRIGGER conversation_activities_cdc_insert;
-- +goose StatementBegin
CREATE TRIGGER conversation_activities_cdc_insert AFTER INSERT ON conversation_activities BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id,s.id,'session_updated',json_object('attentionChanged',json(CASE WHEN NEW.kind IN ('approval','user_input') OR NEW.provider_item_id LIKE 'ao-context-reset:%' THEN 'true' ELSE 'false' END),'id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
  UNION ALL SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
DROP TRIGGER conversation_activities_cdc_update;
-- +goose StatementBegin
CREATE TRIGGER conversation_activities_cdc_update AFTER UPDATE ON conversation_activities WHEN OLD.revision <> NEW.revision BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id,s.id,'session_updated',json_object('attentionChanged',json(CASE WHEN NEW.kind IN ('approval','user_input') OR OLD.kind IN ('approval','user_input') OR NEW.provider_item_id LIKE 'ao-context-reset:%' OR OLD.provider_item_id LIKE 'ao-context-reset:%' THEN 'true' ELSE 'false' END),'id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
  UNION ALL SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
DROP TRIGGER attention_reads_cdc_insert;
-- +goose StatementBegin
CREATE TRIGGER attention_reads_cdc_insert AFTER INSERT ON attention_reads
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('attentionChanged',json('true'),'sessionId',s.id,'conversationId',NEW.conversation_id),NEW.seen_at
 FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
DROP TRIGGER attention_reads_cdc_update;
-- +goose StatementBegin
CREATE TRIGGER attention_reads_cdc_update AFTER UPDATE ON attention_reads
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('attentionChanged',json('true'),'sessionId',s.id,'conversationId',NEW.conversation_id),NEW.seen_at
 FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_report_created AFTER INSERT ON reports WHEN NEW.state IN ('needs_input','stuck') OR EXISTS(SELECT 1 FROM reports previous WHERE previous.session_id=NEW.session_id AND previous.id<>NEW.id AND previous.state IN ('needs_input','stuck')) BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at) VALUES(NEW.project_id,NEW.session_id,'session_updated',json_object('reportId',NEW.id),NEW.created_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_report_updated AFTER UPDATE ON reports WHEN (OLD.state IN ('needs_input','stuck') OR NEW.state IN ('needs_input','stuck')) AND (OLD.note IS NOT NEW.note OR OLD.state IS NOT NEW.state OR OLD.delivery_state IS NOT NEW.delivery_state OR OLD.repeat_count IS NOT NEW.repeat_count) BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at) VALUES(NEW.project_id,NEW.session_id,'session_updated',json_object('reportId',NEW.id),NEW.created_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_report_deleted BEFORE DELETE ON reports BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at) VALUES(OLD.project_id,OLD.session_id,'session_updated',json_object('reportId',OLD.id),OLD.created_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_branch_changed AFTER UPDATE OF active_branch_id,current_session_id ON conversations WHEN OLD.active_branch_id IS NOT NEW.active_branch_id OR OLD.current_session_id IS NOT NEW.current_session_id BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at) SELECT s.project_id,s.id,'session_updated',json_object('attentionChanged',json('true')),NEW.updated_at FROM sessions s WHERE s.id=COALESCE(NEW.current_session_id,OLD.current_session_id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_turn_changed AFTER UPDATE OF state,rolled_back_at ON conversation_turns WHEN (OLD.state IS NOT NEW.state OR OLD.rolled_back_at IS NOT NEW.rolled_back_at) AND EXISTS(SELECT 1 FROM conversation_activities WHERE turn_id=NEW.id AND kind IN ('approval','user_input')) BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at) SELECT s.project_id,s.id,'session_updated',json_object('attentionChanged',json('true')),COALESCE(NEW.completed_at,NEW.started_at,NEW.requested_at) FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_owner_changed AFTER UPDATE OF is_terminated,harness,provider_conversation_id ON sessions WHEN OLD.is_terminated IS NOT NEW.is_terminated OR OLD.harness IS NOT NEW.harness OR OLD.provider_conversation_id IS NOT NEW.provider_conversation_id BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at) VALUES(NEW.project_id,NEW.id,'session_updated',json_object('attentionChanged',json('true')),NEW.updated_at);
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER project_attention_conversation_deleted;
DROP TRIGGER project_attention_session_deleted;
DROP TRIGGER project_attention_provider_deleted;
DROP TRIGGER project_attention_question_deleted;
DROP TRIGGER project_attention_revision_cdc;
DROP TABLE project_attention_revisions;
DROP TRIGGER attention_owner_changed;
DROP TRIGGER attention_turn_changed;
DROP TRIGGER attention_branch_changed;
DROP TRIGGER attention_report_deleted;
DROP TRIGGER attention_report_updated;
DROP TRIGGER attention_report_created;
DROP TRIGGER conversation_activities_cdc_insert;
-- +goose StatementBegin
CREATE TRIGGER conversation_activities_cdc_insert AFTER INSERT ON conversation_activities BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
  UNION ALL SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
DROP TRIGGER conversation_activities_cdc_update;
-- +goose StatementBegin
CREATE TRIGGER conversation_activities_cdc_update AFTER UPDATE ON conversation_activities WHEN OLD.revision <> NEW.revision BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id
  UNION ALL SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',r.id,'conversationId',c.id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),NEW.updated_at FROM conversations c JOIN review r ON r.id=c.current_review_id JOIN sessions s ON s.id=r.session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
DROP TRIGGER attention_reads_cdc_insert;
-- +goose StatementBegin
CREATE TRIGGER attention_reads_cdc_insert AFTER INSERT ON attention_reads
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('sessionId',s.id,'conversationId',NEW.conversation_id),NEW.seen_at
 FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd
DROP TRIGGER attention_reads_cdc_update;
-- +goose StatementBegin
CREATE TRIGGER attention_reads_cdc_update AFTER UPDATE ON attention_reads
BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('sessionId',s.id,'conversationId',NEW.conversation_id),NEW.seen_at
 FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd

DROP TRIGGER attention_report_reads_cdc;
DROP TABLE attention_notification_receipts;
DROP TABLE attention_report_reads;
DROP TABLE attention_report_questions;
