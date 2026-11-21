-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
-- An answer to an AO delivery prompt is recorded only and never sent to the
-- agent. The native-accepted trigger on sessions reads this table, so both
-- triggers are dropped before the rebuild and recreated unchanged afterwards.
PRAGMA foreign_keys=OFF;
BEGIN IMMEDIATE;
DROP TRIGGER attention_answers_native_accepted;
DROP TRIGGER attention_answers_cdc_update;
CREATE TABLE attention_answers_next (
 question_id TEXT PRIMARY KEY REFERENCES attention_questions(id) ON DELETE CASCADE,
 client_answer_id TEXT NOT NULL,
 question_version INTEGER NOT NULL,
 answers_json TEXT NOT NULL CHECK(json_valid(answers_json)),
 fingerprint TEXT NOT NULL,
 message TEXT NOT NULL,
 saved_at DATETIME NOT NULL,
 delivery_state TEXT NOT NULL DEFAULT 'queued' CHECK(delivery_state IN ('queued','delivered','failed','recorded')),
 delivered_at DATETIME,
 last_error TEXT NOT NULL DEFAULT '',
 delivery_attempts INTEGER NOT NULL DEFAULT 0 CHECK(delivery_attempts>=0),
 notes TEXT
);
INSERT INTO attention_answers_next (question_id,client_answer_id,question_version,answers_json,fingerprint,message,saved_at,delivery_state,delivered_at,last_error,delivery_attempts,notes)
 SELECT question_id,client_answer_id,question_version,answers_json,fingerprint,message,saved_at,delivery_state,delivered_at,last_error,delivery_attempts,notes FROM attention_answers;
DROP TABLE attention_answers;
ALTER TABLE attention_answers_next RENAME TO attention_answers;
CREATE TRIGGER attention_answers_cdc_update AFTER UPDATE ON attention_answers
WHEN NEW.delivery_state!=OLD.delivery_state OR NEW.last_error!=OLD.last_error OR NEW.delivered_at IS NOT OLD.delivered_at BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT q.project_id,q.origin_session_id,'session_updated',json_object('sessionId',q.origin_session_id,'questionId',q.id),NEW.saved_at FROM attention_questions q WHERE q.id=NEW.question_id;
END;
CREATE TRIGGER attention_answers_native_accepted AFTER UPDATE OF conversation_checkpoint_state,conversation_checkpoint_generation,conversation_checkpoint_turn_id ON sessions
WHEN NEW.session_mode='tui' AND NEW.is_terminated=0 AND NEW.runtime_launch_id!=''
 AND NEW.conversation_checkpoint_state='coordination' AND NEW.conversation_checkpoint_generation=NEW.runtime_launch_id BEGIN
 UPDATE sessions SET
 latest_user_prompt=(SELECT substr(a.message,1,4096) FROM attention_answers a JOIN attention_questions q ON q.id=a.question_id WHERE 'question-answer-'||q.id=NEW.conversation_checkpoint_turn_id AND q.origin_session_id=NEW.id AND q.origin_harness=NEW.harness AND a.delivery_state='queued'),
 latest_user_prompt_at=(SELECT a.saved_at FROM attention_answers a JOIN attention_questions q ON q.id=a.question_id WHERE 'question-answer-'||q.id=NEW.conversation_checkpoint_turn_id AND q.origin_session_id=NEW.id AND q.origin_harness=NEW.harness AND a.delivery_state='queued'),
 latest_assistant_update=''
 WHERE id=NEW.id AND EXISTS(SELECT 1 FROM attention_answers a JOIN attention_questions q ON q.id=a.question_id WHERE 'question-answer-'||q.id=NEW.conversation_checkpoint_turn_id AND q.origin_session_id=NEW.id AND q.origin_harness=NEW.harness AND a.delivery_state='queued' AND (NEW.latest_user_prompt_at IS NULL OR NEW.latest_user_prompt_at<=a.saved_at));
 UPDATE attention_answers SET delivery_state='delivered',delivered_at=NEW.updated_at,last_error=''
 WHERE delivery_state='queued' AND question_id IN (SELECT id FROM attention_questions WHERE 'question-answer-'||id=NEW.conversation_checkpoint_turn_id AND origin_session_id=NEW.id AND origin_harness=NEW.harness);
END;
COMMIT;
PRAGMA foreign_keys=ON;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;
BEGIN IMMEDIATE;
DROP TRIGGER attention_answers_native_accepted;
DROP TRIGGER attention_answers_cdc_update;
CREATE TABLE attention_answers_next (
 question_id TEXT PRIMARY KEY REFERENCES attention_questions(id) ON DELETE CASCADE,
 client_answer_id TEXT NOT NULL,
 question_version INTEGER NOT NULL,
 answers_json TEXT NOT NULL CHECK(json_valid(answers_json)),
 fingerprint TEXT NOT NULL,
 message TEXT NOT NULL,
 saved_at DATETIME NOT NULL,
 delivery_state TEXT NOT NULL DEFAULT 'queued' CHECK(delivery_state IN ('queued','delivered','failed')),
 delivered_at DATETIME,
 last_error TEXT NOT NULL DEFAULT '',
 delivery_attempts INTEGER NOT NULL DEFAULT 0 CHECK(delivery_attempts>=0),
 notes TEXT
);
INSERT INTO attention_answers_next (question_id,client_answer_id,question_version,answers_json,fingerprint,message,saved_at,delivery_state,delivered_at,last_error,delivery_attempts,notes)
 SELECT question_id,client_answer_id,question_version,answers_json,fingerprint,message,saved_at,CASE delivery_state WHEN 'recorded' THEN 'failed' ELSE delivery_state END,delivered_at,last_error,delivery_attempts,notes FROM attention_answers;
DROP TABLE attention_answers;
ALTER TABLE attention_answers_next RENAME TO attention_answers;
CREATE TRIGGER attention_answers_cdc_update AFTER UPDATE ON attention_answers
WHEN NEW.delivery_state!=OLD.delivery_state OR NEW.last_error!=OLD.last_error OR NEW.delivered_at IS NOT OLD.delivered_at BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT q.project_id,q.origin_session_id,'session_updated',json_object('sessionId',q.origin_session_id,'questionId',q.id),NEW.saved_at FROM attention_questions q WHERE q.id=NEW.question_id;
END;
CREATE TRIGGER attention_answers_native_accepted AFTER UPDATE OF conversation_checkpoint_state,conversation_checkpoint_generation,conversation_checkpoint_turn_id ON sessions
WHEN NEW.session_mode='tui' AND NEW.is_terminated=0 AND NEW.runtime_launch_id!=''
 AND NEW.conversation_checkpoint_state='coordination' AND NEW.conversation_checkpoint_generation=NEW.runtime_launch_id BEGIN
 UPDATE sessions SET
 latest_user_prompt=(SELECT substr(a.message,1,4096) FROM attention_answers a JOIN attention_questions q ON q.id=a.question_id WHERE 'question-answer-'||q.id=NEW.conversation_checkpoint_turn_id AND q.origin_session_id=NEW.id AND q.origin_harness=NEW.harness AND a.delivery_state='queued'),
 latest_user_prompt_at=(SELECT a.saved_at FROM attention_answers a JOIN attention_questions q ON q.id=a.question_id WHERE 'question-answer-'||q.id=NEW.conversation_checkpoint_turn_id AND q.origin_session_id=NEW.id AND q.origin_harness=NEW.harness AND a.delivery_state='queued'),
 latest_assistant_update=''
 WHERE id=NEW.id AND EXISTS(SELECT 1 FROM attention_answers a JOIN attention_questions q ON q.id=a.question_id WHERE 'question-answer-'||q.id=NEW.conversation_checkpoint_turn_id AND q.origin_session_id=NEW.id AND q.origin_harness=NEW.harness AND a.delivery_state='queued' AND (NEW.latest_user_prompt_at IS NULL OR NEW.latest_user_prompt_at<=a.saved_at));
 UPDATE attention_answers SET delivery_state='delivered',delivered_at=NEW.updated_at,last_error=''
 WHERE delivery_state='queued' AND question_id IN (SELECT id FROM attention_questions WHERE 'question-answer-'||id=NEW.conversation_checkpoint_turn_id AND origin_session_id=NEW.id AND origin_harness=NEW.harness);
END;
COMMIT;
PRAGMA foreign_keys=ON;
-- +goose StatementEnd
