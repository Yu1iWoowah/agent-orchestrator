-- +goose Up
CREATE TABLE attention_questions (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE,
 project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
 origin_session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 origin_harness TEXT NOT NULL,
 client_key TEXT NOT NULL,
 audience TEXT NOT NULL CHECK(audience='user'),
 kind TEXT NOT NULL CHECK(kind IN ('question','authorization')),
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json)),
 create_fingerprint TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'open' CHECK(state IN ('open','answered','cancelled','expired')),
 version INTEGER NOT NULL DEFAULT 1 CHECK(version BETWEEN 1 AND 9007199254740991),
 supersedes_id TEXT REFERENCES attention_questions(id),
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL,
 expires_at DATETIME,
 closed_at DATETIME,
 closed_by TEXT NOT NULL DEFAULT '',
 close_reason TEXT NOT NULL DEFAULT '',
 seen_version INTEGER NOT NULL DEFAULT 0,
 seen_at DATETIME,
 UNIQUE(origin_session_id,client_key)
);
CREATE INDEX attention_questions_session_page ON attention_questions(origin_session_id,sequence DESC);
CREATE INDEX attention_questions_expiration ON attention_questions(expires_at) WHERE state='open';
CREATE TABLE attention_answers (
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
 delivery_attempts INTEGER NOT NULL DEFAULT 0 CHECK(delivery_attempts>=0)
);
-- +goose StatementBegin
CREATE TRIGGER attention_questions_cdc_insert AFTER INSERT ON attention_questions BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 VALUES(NEW.project_id,NEW.origin_session_id,'session_updated',json_object('sessionId',NEW.origin_session_id,'questionId',NEW.id,'conversationId',(SELECT id FROM conversations WHERE current_session_id=NEW.origin_session_id ORDER BY scope='project' DESC LIMIT 1)),NEW.updated_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_questions_cdc_update AFTER UPDATE ON attention_questions BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 VALUES(NEW.project_id,NEW.origin_session_id,'session_updated',json_object('sessionId',NEW.origin_session_id,'questionId',NEW.id,'conversationId',(SELECT id FROM conversations WHERE current_session_id=NEW.origin_session_id ORDER BY scope='project' DESC LIMIT 1)),NEW.updated_at);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_answers_cdc_update AFTER UPDATE ON attention_answers
WHEN NEW.delivery_state!=OLD.delivery_state OR NEW.last_error!=OLD.last_error OR NEW.delivered_at IS NOT OLD.delivered_at BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT q.project_id,q.origin_session_id,'session_updated',json_object('sessionId',q.origin_session_id,'questionId',q.id),NEW.saved_at FROM attention_questions q WHERE q.id=NEW.question_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_questions_session_terminated AFTER UPDATE OF is_terminated ON sessions
WHEN NEW.is_terminated=1 AND OLD.is_terminated=0 BEGIN
 UPDATE attention_questions SET state='expired',closed_at=NEW.updated_at,updated_at=NEW.updated_at,closed_by='system',close_reason='origin_session_terminated' WHERE origin_session_id=NEW.id AND state='open';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attention_questions_agent_replaced AFTER UPDATE OF harness ON sessions
WHEN NEW.harness!=OLD.harness BEGIN
 UPDATE attention_questions SET state='expired',closed_at=NEW.updated_at,updated_at=NEW.updated_at,closed_by='system',close_reason='origin_agent_replaced' WHERE origin_session_id=NEW.id AND origin_harness!=NEW.harness AND state='open';
END;
-- +goose StatementEnd
-- The correlated native checkpoint is durable acceptance, including a crash
-- before the outbox coordinator receives its acknowledgment. Do not depend on
-- that checkpoint remaining the session's latest turn after the transaction.
-- +goose StatementBegin
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
-- +goose StatementEnd
-- +goose Down
DROP TRIGGER attention_answers_native_accepted;
DROP TRIGGER attention_questions_agent_replaced;
DROP TRIGGER attention_questions_session_terminated;
DROP TRIGGER attention_answers_cdc_update;
DROP TRIGGER attention_questions_cdc_update;
DROP TRIGGER attention_questions_cdc_insert;
DROP TABLE attention_answers;
DROP TABLE attention_questions;
