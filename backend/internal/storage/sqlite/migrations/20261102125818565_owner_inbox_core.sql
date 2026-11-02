-- +goose Up
-- Disabled H core: no backfill, no legacy claim/input/ACK cutover.
CREATE INDEX owner_inbox_held_inputs ON conversation_turns(id) WHERE state='held';
CREATE TABLE project_orchestrator_owners (
 project_id TEXT PRIMARY KEY REFERENCES projects(id),
 session_id TEXT NOT NULL REFERENCES sessions(id),
 epoch INTEGER NOT NULL CHECK(epoch>0),
 conversation_id TEXT NOT NULL REFERENCES conversations(id),
 branch_id TEXT NOT NULL,
 generation TEXT NOT NULL,
 policy_revision INTEGER NOT NULL CHECK(policy_revision>0)
) WITHOUT ROWID;

CREATE TABLE owner_inbox_sources (
 report_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL REFERENCES projects(id),
 sender_id TEXT NOT NULL,
 caller_key TEXT NOT NULL,
 fingerprint TEXT NOT NULL,
 source_version INTEGER NOT NULL CHECK(source_version>0),
 report_json TEXT NOT NULL CHECK(json_valid(report_json)),
 acknowledged_at DATETIME,
 CHECK(json_extract(report_json,'$.ID') IS report_id
   AND json_extract(report_json,'$.ProjectID') IS project_id
   AND json_extract(report_json,'$.SessionID') IS sender_id),
 UNIQUE(project_id,sender_id,caller_key)
) WITHOUT ROWID;

CREATE TABLE owner_inbox_batches (
 project_id TEXT NOT NULL REFERENCES projects(id),
 batch_key TEXT NOT NULL,
 plan_hash TEXT NOT NULL,
 text_hash TEXT NOT NULL,
 policy_revision INTEGER NOT NULL CHECK(policy_revision>0),
 PRIMARY KEY(project_id,batch_key)
) WITHOUT ROWID;

CREATE TABLE owner_inbox_members (
 report_id TEXT PRIMARY KEY REFERENCES owner_inbox_sources(report_id),
 project_id TEXT NOT NULL,
 batch_key TEXT NOT NULL,
 position INTEGER NOT NULL CHECK(position>=0),
 source_version INTEGER NOT NULL CHECK(source_version>0),
 UNIQUE(project_id,batch_key,position),
 FOREIGN KEY(project_id,batch_key) REFERENCES owner_inbox_batches(project_id,batch_key)
) WITHOUT ROWID;

CREATE TABLE owner_inbox_actions (
 project_id TEXT NOT NULL REFERENCES projects(id),
 action_key TEXT NOT NULL,
 stage TEXT NOT NULL CHECK(stage IN ('accept','claim','bind')),
 fingerprint TEXT NOT NULL,
 receipt_json TEXT NOT NULL CHECK(json_valid(receipt_json)),
 PRIMARY KEY(project_id,action_key)
) WITHOUT ROWID;

CREATE TABLE owner_inbox_acceptances (
 project_id TEXT NOT NULL,
 batch_key TEXT NOT NULL,
 turn_id TEXT NOT NULL UNIQUE REFERENCES conversation_turns(id),
 action_key TEXT NOT NULL,
 owner_epoch INTEGER NOT NULL CHECK(owner_epoch>0),
 intake_receipt_ref TEXT NOT NULL,
 PRIMARY KEY(project_id,batch_key),
 FOREIGN KEY(project_id,batch_key) REFERENCES owner_inbox_batches(project_id,batch_key),
 FOREIGN KEY(project_id,action_key) REFERENCES owner_inbox_actions(project_id,action_key)
) WITHOUT ROWID;

-- Receipts and fixed membership survive cancellation, return and retries.
-- +goose StatementBegin
CREATE TRIGGER owner_inbox_action_no_update BEFORE UPDATE ON owner_inbox_actions BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_RECEIPT_IMMUTABLE');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER owner_inbox_action_no_delete BEFORE DELETE ON owner_inbox_actions BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_RECEIPT_IMMUTABLE');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER owner_inbox_action_cdc AFTER INSERT ON owner_inbox_actions BEGIN
 INSERT INTO change_log(project_id,event_type,payload,created_at)
 VALUES(NEW.project_id,'session_updated',json_object('ownerInboxChanged',json('true'),'stage',NEW.stage),CURRENT_TIMESTAMP);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_sources_update_fence BEFORE UPDATE OF report_id,project_id,sender_id,caller_key,fingerprint,source_version,report_json ON owner_inbox_sources BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_sources_delete_fence BEFORE DELETE ON owner_inbox_sources BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_sources_replace_fence BEFORE INSERT ON owner_inbox_sources
WHEN NEW.acknowledged_at IS NOT NULL
 OR EXISTS(SELECT 1 FROM owner_inbox_sources WHERE report_id=NEW.report_id OR (project_id=NEW.project_id AND sender_id=NEW.sender_id AND caller_key=NEW.caller_key))
 OR EXISTS(SELECT 1 FROM reports WHERE id=NEW.report_id) BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_batches_update_fence BEFORE UPDATE ON owner_inbox_batches BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_batches_delete_fence BEFORE DELETE ON owner_inbox_batches BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_batches_replace_fence BEFORE INSERT ON owner_inbox_batches
WHEN EXISTS(SELECT 1 FROM owner_inbox_batches WHERE project_id=NEW.project_id AND batch_key=NEW.batch_key) BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_members_update_fence BEFORE UPDATE ON owner_inbox_members BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_members_delete_fence BEFORE DELETE ON owner_inbox_members BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_members_replace_fence BEFORE INSERT ON owner_inbox_members
WHEN EXISTS(SELECT 1 FROM owner_inbox_members WHERE report_id=NEW.report_id OR (project_id=NEW.project_id AND batch_key=NEW.batch_key AND position=NEW.position)) BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_acceptances_update_fence BEFORE UPDATE ON owner_inbox_acceptances BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_acceptances_delete_fence BEFORE DELETE ON owner_inbox_acceptances BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_acceptances_replace_fence BEFORE INSERT ON owner_inbox_acceptances
WHEN EXISTS(SELECT 1 FROM owner_inbox_acceptances WHERE (project_id=NEW.project_id AND batch_key=NEW.batch_key) OR turn_id=NEW.turn_id) BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_action_replace_fence BEFORE INSERT ON owner_inbox_actions
WHEN EXISTS(SELECT 1 FROM owner_inbox_actions WHERE project_id=NEW.project_id AND action_key=NEW.action_key) BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_RECEIPT_IMMUTABLE');
END;
-- +goose StatementEnd

-- H sources live outside reports: old binaries cannot schedule or claim them.
-- +goose StatementBegin
CREATE TRIGGER owner_inbox_report_ack_fence BEFORE UPDATE OF acknowledged_at ON owner_inbox_sources
WHEN NOT (OLD.acknowledged_at IS NULL AND NEW.acknowledged_at IS NOT NULL
 AND EXISTS(SELECT 1 FROM owner_inbox_members m JOIN owner_inbox_acceptances a ON a.project_id=m.project_id AND a.batch_key=m.batch_key WHERE m.report_id=OLD.report_id AND m.project_id=OLD.project_id)) BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_ACK_FENCED');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER owner_inbox_report_replace_fence BEFORE INSERT ON reports
WHEN EXISTS(SELECT 1 FROM owner_inbox_sources WHERE report_id=NEW.id) BEGIN
 SELECT RAISE(ABORT,'OWNER_INBOX_SOURCE_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose Down
-- Binary backout never performs Down. Refuse schema removal after first use.
CREATE TEMP TABLE owner_inbox_down_check(ok INTEGER CHECK(ok=1));
INSERT INTO owner_inbox_down_check SELECT
 NOT EXISTS(SELECT 1 FROM owner_inbox_actions)
 AND NOT EXISTS(SELECT 1 FROM project_orchestrator_owners)
 AND NOT EXISTS(SELECT 1 FROM owner_inbox_sources)
 AND NOT EXISTS(SELECT 1 FROM owner_inbox_batches);
DROP TABLE owner_inbox_down_check;
DROP TRIGGER owner_inbox_sources_update_fence;
DROP TRIGGER owner_inbox_sources_delete_fence;
DROP TRIGGER owner_inbox_sources_replace_fence;
DROP TRIGGER owner_inbox_batches_update_fence;
DROP TRIGGER owner_inbox_batches_delete_fence;
DROP TRIGGER owner_inbox_batches_replace_fence;
DROP TRIGGER owner_inbox_members_update_fence;
DROP TRIGGER owner_inbox_members_delete_fence;
DROP TRIGGER owner_inbox_members_replace_fence;
DROP TRIGGER owner_inbox_acceptances_update_fence;
DROP TRIGGER owner_inbox_acceptances_delete_fence;
DROP TRIGGER owner_inbox_acceptances_replace_fence;
DROP TRIGGER owner_inbox_action_replace_fence;
DROP TRIGGER owner_inbox_report_ack_fence;
DROP TRIGGER owner_inbox_report_replace_fence;
DROP TRIGGER owner_inbox_action_cdc;
DROP TRIGGER owner_inbox_action_no_delete;
DROP TRIGGER owner_inbox_action_no_update;
DROP TABLE owner_inbox_acceptances;
DROP TABLE owner_inbox_actions;
DROP TABLE owner_inbox_members;
DROP TABLE owner_inbox_batches;
DROP TABLE owner_inbox_sources;
DROP TABLE project_orchestrator_owners;
DROP INDEX owner_inbox_held_inputs;
