-- +goose Up
-- +goose StatementBegin
-- Goose commits schema and its migration ledger in the same transaction.
-- migrateUpTo disables FK enforcement before BEGIN; the check below rejects
-- direct Goose calls that could cascade-delete input relationships.
CREATE TEMP TABLE shared_input_fk_mode(ok INTEGER CHECK(ok=1));
INSERT INTO shared_input_fk_mode SELECT foreign_keys=0 FROM pragma_foreign_keys;
DROP TABLE shared_input_fk_mode;
-- Preserve legacy violations; inspect only the rebuilt table, its children,
-- and shared custody tables. Materialize names before scanning their rows.
CREATE TEMP TABLE shared_input_fk_tables(name TEXT PRIMARY KEY);
INSERT INTO shared_input_fk_tables
 SELECT name FROM main.sqlite_master m WHERE type='table' AND (
  name IN ('conversation_turns','conversation_input_custody','conversation_input_holds')
  OR EXISTS(SELECT 1 FROM pragma_foreign_key_list(m.name) fk WHERE fk."table"='conversation_turns')
 );
CREATE TEMP TABLE shared_input_fk_before("table" TEXT,rowid INTEGER,parent TEXT,fkid INTEGER);
INSERT INTO shared_input_fk_before
SELECT fk."table",fk.rowid,fk.parent,fk.fkid
 FROM shared_input_fk_tables t JOIN pragma_foreign_key_check(t.name) fk;
DROP TRIGGER attention_turn_changed;
DROP TRIGGER conversation_turn_settings_snapshot_cdc;
DROP TRIGGER conversation_turns_auto_resume_cdc_update;
DROP TRIGGER conversation_turns_branch_insert;
DROP TRIGGER conversation_turns_cdc_update;
DROP TRIGGER replacement_intake_delete;
DROP TRIGGER replacement_intake_insert;
DROP TRIGGER replacement_intake_turn_settled;
DROP TRIGGER replacement_intake_update;
DROP TRIGGER replacement_notice_history_no_delete;
DROP TRIGGER replacement_notice_history_no_update;
DROP TRIGGER replacement_notice_identity;
DROP TRIGGER replacement_notice_no_delete;
DROP TRIGGER replacement_orchestrator_insert_fence;
DROP TRIGGER replacement_orchestrator_restore_fence;
DROP TRIGGER replacement_repair_snapshot_no_delete;
DROP TRIGGER replacement_repair_snapshot_no_update;
DROP TRIGGER replacement_report_intake_delete;
DROP TRIGGER replacement_report_intake_insert;
DROP TRIGGER replacement_report_intake_update;
DROP TRIGGER replacement_successor_bind;
DROP TRIGGER replacement_successor_delete_fence;
DROP TRIGGER replacement_turn_dispatch_fence;
DROP VIEW orchestrator_replacement_held_turns;
DROP VIEW orchestrator_replacement_released_sessions;
CREATE TABLE conversation_turns_next (
    id                     TEXT PRIMARY KEY,
    conversation_id        TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    handled_by_session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    provider_turn_id       TEXT NOT NULL DEFAULT '',
    controller_generation  TEXT NOT NULL DEFAULT '',
    state                  TEXT NOT NULL CHECK (state IN ('queued', 'running', 'completed', 'recovered', 'interrupted', 'failed', 'cancelled', 'held')),
    error_message          TEXT NOT NULL DEFAULT '',
    requested_at           TIMESTAMP NOT NULL,
    started_at             TIMESTAMP,
    completed_at           TIMESTAMP,
    diff_json              TEXT NOT NULL DEFAULT '',
    rolled_back_at         TIMESTAMP,
    plan_json              TEXT NOT NULL DEFAULT '',
    branch_id              TEXT NOT NULL DEFAULT '',
    promotion_started_at   TIMESTAMP,
    promoted_to_turn_id    TEXT REFERENCES conversation_turns_next(id) ON DELETE SET NULL,
    retry_of_turn_id       TEXT REFERENCES conversation_turns_next(id) ON DELETE RESTRICT
, handled_by_review_id TEXT REFERENCES review(id) ON DELETE SET NULL, usage_limit_resets_at TIMESTAMP, usage_limit_auto_resume INTEGER NOT NULL DEFAULT 0 CHECK (usage_limit_auto_resume IN (0, 1)), usage_limit_resume_error TEXT NOT NULL DEFAULT '', batched_input INTEGER NOT NULL DEFAULT 0, provider_input_text TEXT NOT NULL DEFAULT '', intake_sequence INTEGER NOT NULL DEFAULT 0 CHECK(intake_sequence >= 0), agent_settings_snapshot_json TEXT NOT NULL DEFAULT ''
CHECK (agent_settings_snapshot_json='' OR json_valid(agent_settings_snapshot_json)));
INSERT INTO conversation_turns_next(rowid, id,conversation_id,handled_by_session_id,provider_turn_id,controller_generation,state,error_message,requested_at,started_at,completed_at,diff_json,rolled_back_at,plan_json,branch_id,promotion_started_at,promoted_to_turn_id,retry_of_turn_id,handled_by_review_id,usage_limit_resets_at,usage_limit_auto_resume,usage_limit_resume_error,batched_input,provider_input_text,intake_sequence,agent_settings_snapshot_json) SELECT rowid, id,conversation_id,handled_by_session_id,provider_turn_id,controller_generation,state,error_message,requested_at,started_at,completed_at,diff_json,rolled_back_at,plan_json,branch_id,promotion_started_at,promoted_to_turn_id,retry_of_turn_id,handled_by_review_id,usage_limit_resets_at,usage_limit_auto_resume,usage_limit_resume_error,batched_input,provider_input_text,intake_sequence,agent_settings_snapshot_json FROM conversation_turns ORDER BY rowid;
DROP TABLE conversation_turns;
ALTER TABLE conversation_turns_next RENAME TO conversation_turns;
CREATE INDEX conversation_turns_auto_resume ON conversation_turns(usage_limit_resets_at) WHERE usage_limit_auto_resume = 1;
CREATE INDEX idx_conversation_turns_branch
    ON conversation_turns(branch_id, requested_at);
CREATE INDEX idx_conversation_turns_conversation
    ON conversation_turns(conversation_id, requested_at);
CREATE UNIQUE INDEX idx_conversation_turns_provider
    ON conversation_turns(conversation_id, provider_turn_id)
    WHERE provider_turn_id <> '';
CREATE INDEX idx_conversation_turns_retry_source
    ON conversation_turns(conversation_id, retry_of_turn_id)
    WHERE retry_of_turn_id IS NOT NULL;
-- Custody is routing, not delivery evidence. Holds retain their ledger owner
-- and version across custody moves; only their producer may authorize release.
CREATE TABLE conversation_input_actions (
 action_key TEXT PRIMARY KEY CHECK(length(action_key) BETWEEN 1 AND 256),
 turn_id TEXT NOT NULL CHECK(length(turn_id) BETWEEN 1 AND 256),
 kind TEXT NOT NULL CHECK(kind IN ('register','hold','release','transfer','delete')),
 expected_owner TEXT NOT NULL,
 expected_revision INTEGER NOT NULL CHECK(expected_revision >= 0),
 target_owner TEXT NOT NULL DEFAULT '',
 hold_key TEXT NOT NULL DEFAULT '',
 hold_owner TEXT NOT NULL DEFAULT '',
 hold_version INTEGER NOT NULL DEFAULT 0 CHECK(hold_version >= 0),
 reason TEXT NOT NULL DEFAULT '',
 target_state TEXT NOT NULL DEFAULT '' CHECK(target_state IN ('','queued','running','completed','recovered','interrupted','failed','cancelled')),
 created_at TIMESTAMP NOT NULL,
 CHECK(length(expected_owner) <= 256 AND length(target_owner) <= 256 AND length(hold_key) <= 256 AND length(hold_owner) <= 256 AND length(reason) <= 128)
);
CREATE INDEX conversation_input_actions_turn ON conversation_input_actions(turn_id, expected_revision);
CREATE TABLE conversation_input_custody (
 turn_id TEXT PRIMARY KEY REFERENCES conversation_turns(id) ON DELETE CASCADE,
 owner TEXT NOT NULL CHECK(length(owner) BETWEEN 1 AND 256),
 revision INTEGER NOT NULL CHECK(revision > 0),
 original_state TEXT NOT NULL CHECK(original_state IN ('queued','running','completed','recovered','interrupted','failed','cancelled')),
 original_conversation_id TEXT NOT NULL,
 original_branch_id TEXT NOT NULL,
 original_session_id TEXT NOT NULL,
 original_generation TEXT NOT NULL,
 original_sequence INTEGER NOT NULL,
 requested_at TIMESTAMP NOT NULL,
 last_action_key TEXT NOT NULL REFERENCES conversation_input_actions(action_key)
);
CREATE TABLE conversation_input_holds (
 turn_id TEXT NOT NULL REFERENCES conversation_input_custody(turn_id) ON DELETE CASCADE,
 hold_key TEXT NOT NULL CHECK(length(hold_key) BETWEEN 1 AND 256),
 ledger_owner TEXT NOT NULL CHECK(length(ledger_owner) BETWEEN 1 AND 256),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 128),
 version INTEGER NOT NULL CHECK(version > 0),
 active INTEGER NOT NULL CHECK(active IN (0,1)),
 last_action_key TEXT NOT NULL REFERENCES conversation_input_actions(action_key),
 PRIMARY KEY(turn_id, hold_key)
);
CREATE INDEX conversation_input_holds_active ON conversation_input_holds(turn_id) WHERE active = 1;

CREATE TRIGGER conversation_input_action_validate BEFORE INSERT ON conversation_input_actions
BEGIN
 SELECT CASE WHEN EXISTS(SELECT 1 FROM conversation_input_actions WHERE action_key=NEW.action_key) THEN RAISE(ABORT,'INPUT_ACTION_IMMUTABLE') END;
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM conversation_turns WHERE id = NEW.turn_id)
  THEN RAISE(ABORT,'INPUT_CUSTODY_TURN_MISSING') END;
 SELECT CASE WHEN NEW.kind = 'register' AND (NEW.expected_revision <> 0 OR NEW.expected_owner <> '' OR NEW.target_owner = ''
  OR EXISTS(SELECT 1 FROM conversation_input_custody WHERE turn_id = NEW.turn_id)
  OR EXISTS(SELECT 1 FROM conversation_turns WHERE id = NEW.turn_id AND state = 'held'))
  THEN RAISE(ABORT,'INPUT_CUSTODY_CONFLICT') END;
 SELECT CASE WHEN NEW.kind <> 'register' AND NOT EXISTS(SELECT 1 FROM conversation_input_custody
  WHERE turn_id = NEW.turn_id AND owner = NEW.expected_owner AND revision = NEW.expected_revision)
  THEN RAISE(ABORT,'INPUT_CUSTODY_CONFLICT') END;
 SELECT CASE WHEN NEW.kind = 'hold' AND (NEW.hold_key = '' OR NEW.reason = '' OR NEW.hold_owner <> NEW.expected_owner
  OR NEW.hold_version <> COALESCE((SELECT version FROM conversation_input_holds WHERE turn_id=NEW.turn_id AND hold_key=NEW.hold_key),0)
  OR EXISTS(SELECT 1 FROM conversation_input_holds WHERE turn_id=NEW.turn_id AND hold_key=NEW.hold_key AND active=1))
  THEN RAISE(ABORT,'INPUT_HOLD_CONFLICT') END;
 SELECT CASE WHEN NEW.kind = 'release' AND (NEW.target_state = '' OR NOT EXISTS(SELECT 1 FROM conversation_input_holds
  WHERE turn_id=NEW.turn_id AND hold_key=NEW.hold_key AND ledger_owner=NEW.hold_owner AND version=NEW.hold_version AND active=1))
  THEN RAISE(ABORT,'INPUT_HOLD_CONFLICT') END;
 SELECT CASE WHEN NEW.kind = 'transfer' AND (NEW.target_owner = '' OR NEW.target_owner = NEW.expected_owner)
  THEN RAISE(ABORT,'INPUT_CUSTODY_CONFLICT') END;
END;
CREATE TRIGGER conversation_input_action_no_update BEFORE UPDATE ON conversation_input_actions
BEGIN SELECT RAISE(ABORT,'INPUT_ACTION_IMMUTABLE'); END;
CREATE TRIGGER conversation_input_action_no_delete BEFORE DELETE ON conversation_input_actions
BEGIN SELECT RAISE(ABORT,'INPUT_ACTION_IMMUTABLE'); END;
CREATE TRIGGER conversation_input_custody_insert BEFORE INSERT ON conversation_input_custody
WHEN EXISTS(SELECT 1 FROM conversation_input_custody WHERE turn_id=NEW.turn_id)
 OR NOT EXISTS(SELECT 1 FROM conversation_input_actions a JOIN conversation_turns t ON t.id=a.turn_id
 WHERE a.action_key=NEW.last_action_key AND a.kind='register' AND a.turn_id=NEW.turn_id AND a.target_owner=NEW.owner
 AND NEW.revision=1 AND NEW.original_state=t.state AND NEW.original_conversation_id=t.conversation_id
 AND NEW.original_branch_id=t.branch_id AND NEW.original_session_id=t.handled_by_session_id
 AND NEW.original_generation=t.controller_generation AND NEW.original_sequence=t.intake_sequence AND NEW.requested_at=t.requested_at)
BEGIN SELECT RAISE(ABORT,'INPUT_CUSTODY_FENCED'); END;
CREATE TRIGGER conversation_input_custody_update BEFORE UPDATE ON conversation_input_custody
WHEN NEW.turn_id IS NOT OLD.turn_id OR NEW.original_state IS NOT OLD.original_state
 OR NEW.original_conversation_id IS NOT OLD.original_conversation_id OR NEW.original_branch_id IS NOT OLD.original_branch_id
 OR NEW.original_session_id IS NOT OLD.original_session_id OR NEW.original_generation IS NOT OLD.original_generation
 OR NEW.original_sequence IS NOT OLD.original_sequence OR NEW.requested_at IS NOT OLD.requested_at
 OR NOT EXISTS(SELECT 1 FROM conversation_input_actions a WHERE a.action_key=NEW.last_action_key AND a.turn_id=OLD.turn_id
  AND a.kind <> 'register' AND a.expected_owner=OLD.owner AND a.expected_revision=OLD.revision AND NEW.revision=OLD.revision+1
  AND NEW.owner=CASE WHEN a.kind='transfer' THEN a.target_owner ELSE OLD.owner END)
BEGIN SELECT RAISE(ABORT,'INPUT_CUSTODY_FENCED'); END;
CREATE TRIGGER conversation_input_custody_delete BEFORE DELETE ON conversation_input_custody
WHEN NOT EXISTS(SELECT 1 FROM conversation_input_actions WHERE action_key=OLD.last_action_key AND turn_id=OLD.turn_id AND kind='delete')
BEGIN SELECT RAISE(ABORT,'INPUT_CUSTODY_FENCED'); END;
CREATE TRIGGER conversation_input_hold_insert BEFORE INSERT ON conversation_input_holds
WHEN NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN conversation_input_actions a ON a.action_key=c.last_action_key
 WHERE c.turn_id=NEW.turn_id AND a.kind='hold' AND a.action_key=NEW.last_action_key AND a.hold_key=NEW.hold_key
 AND a.hold_owner=NEW.ledger_owner AND a.reason=NEW.reason AND NEW.version=a.hold_version+1 AND NEW.active=1)
BEGIN SELECT RAISE(ABORT,'INPUT_HOLD_FENCED'); END;
CREATE TRIGGER conversation_input_hold_update BEFORE UPDATE ON conversation_input_holds
WHEN NEW.turn_id IS NOT OLD.turn_id OR NEW.hold_key IS NOT OLD.hold_key
 OR NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN conversation_input_actions a ON a.action_key=c.last_action_key
 WHERE c.turn_id=OLD.turn_id AND a.action_key=NEW.last_action_key AND a.hold_key=OLD.hold_key
 AND a.hold_version=OLD.version AND NEW.version=OLD.version+1
 AND ((a.kind='hold' AND OLD.active=0 AND NEW.active=1 AND a.hold_owner=NEW.ledger_owner AND a.reason=NEW.reason)
 OR (a.kind='release' AND OLD.active=1 AND NEW.active=0 AND NEW.ledger_owner=OLD.ledger_owner AND NEW.reason=OLD.reason)))
BEGIN SELECT RAISE(ABORT,'INPUT_HOLD_FENCED'); END;
CREATE TRIGGER conversation_input_hold_delete BEFORE DELETE ON conversation_input_holds
WHEN NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN conversation_input_actions a ON a.action_key=c.last_action_key
 WHERE c.turn_id=OLD.turn_id AND a.kind='delete')
BEGIN SELECT RAISE(ABORT,'INPUT_HOLD_FENCED'); END;

CREATE TRIGGER conversation_input_turn_insert BEFORE INSERT ON conversation_turns WHEN NEW.state='held'
 OR EXISTS(SELECT 1 FROM conversation_turns WHERE id=NEW.id AND state='held')
BEGIN SELECT RAISE(ABORT,'INPUT_HOLD_REQUIRED'); END;
CREATE TRIGGER conversation_input_turn_update BEFORE UPDATE ON conversation_turns
WHEN (OLD.state='held' OR NEW.state='held' OR EXISTS(SELECT 1 FROM conversation_input_holds WHERE turn_id=OLD.id AND active=1))
 AND ((OLD.id IS NOT NEW.id OR OLD.conversation_id IS NOT NEW.conversation_id OR OLD.handled_by_session_id IS NOT NEW.handled_by_session_id OR OLD.provider_turn_id IS NOT NEW.provider_turn_id OR OLD.controller_generation IS NOT NEW.controller_generation OR OLD.error_message IS NOT NEW.error_message OR OLD.requested_at IS NOT NEW.requested_at OR OLD.started_at IS NOT NEW.started_at OR OLD.completed_at IS NOT NEW.completed_at OR OLD.diff_json IS NOT NEW.diff_json OR OLD.rolled_back_at IS NOT NEW.rolled_back_at OR OLD.plan_json IS NOT NEW.plan_json OR OLD.branch_id IS NOT NEW.branch_id OR OLD.promotion_started_at IS NOT NEW.promotion_started_at OR OLD.promoted_to_turn_id IS NOT NEW.promoted_to_turn_id OR OLD.retry_of_turn_id IS NOT NEW.retry_of_turn_id OR OLD.handled_by_review_id IS NOT NEW.handled_by_review_id OR OLD.usage_limit_resets_at IS NOT NEW.usage_limit_resets_at OR OLD.usage_limit_auto_resume IS NOT NEW.usage_limit_auto_resume OR OLD.usage_limit_resume_error IS NOT NEW.usage_limit_resume_error OR OLD.batched_input IS NOT NEW.batched_input OR OLD.provider_input_text IS NOT NEW.provider_input_text OR OLD.intake_sequence IS NOT NEW.intake_sequence OR OLD.agent_settings_snapshot_json IS NOT NEW.agent_settings_snapshot_json)
 OR NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN conversation_input_actions a ON a.action_key=c.last_action_key
 WHERE c.turn_id=OLD.id AND ((a.kind='hold' AND NEW.state='held' AND EXISTS(SELECT 1 FROM conversation_input_holds h WHERE h.turn_id=OLD.id AND h.active=1))
 OR (a.kind='release' AND NEW.state=a.target_state AND NOT EXISTS(SELECT 1 FROM conversation_input_holds h WHERE h.turn_id=OLD.id AND h.active=1)))))
BEGIN SELECT RAISE(ABORT,'INPUT_HELD'); END;
CREATE TRIGGER conversation_input_turn_delete BEFORE DELETE ON conversation_turns
WHEN EXISTS(SELECT 1 FROM conversation_input_custody WHERE turn_id=OLD.id)
 AND NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN conversation_input_actions a ON a.action_key=c.last_action_key
 WHERE c.turn_id=OLD.id AND a.kind='delete')
BEGIN SELECT RAISE(ABORT,'INPUT_HELD'); END;

CREATE TRIGGER conversation_input_message_insert BEFORE INSERT ON conversation_messages
WHEN EXISTS(SELECT 1 FROM conversation_input_holds WHERE turn_id=NEW.turn_id AND active=1)
 OR EXISTS(SELECT 1 FROM conversation_messages m JOIN conversation_input_holds h ON h.turn_id=m.turn_id WHERE m.id=NEW.id AND h.active=1)
BEGIN SELECT RAISE(ABORT,'INPUT_HELD'); END;
CREATE TRIGGER conversation_input_message_update BEFORE UPDATE ON conversation_messages
WHEN EXISTS(SELECT 1 FROM conversation_input_holds h JOIN conversation_input_custody c ON c.turn_id=h.turn_id
 JOIN conversation_input_actions a ON a.action_key=c.last_action_key
 WHERE h.turn_id IN (OLD.turn_id,NEW.turn_id) AND h.active=1 AND a.kind<>'delete')
BEGIN SELECT RAISE(ABORT,'INPUT_HELD'); END;
CREATE TRIGGER conversation_input_message_delete BEFORE DELETE ON conversation_messages
WHEN EXISTS(SELECT 1 FROM conversation_input_holds h JOIN conversation_input_custody c ON c.turn_id=h.turn_id
 JOIN conversation_input_actions a ON a.action_key=c.last_action_key
 WHERE h.turn_id=OLD.turn_id AND h.active=1 AND a.kind<>'delete')
BEGIN SELECT RAISE(ABORT,'INPUT_HELD'); END;
CREATE TRIGGER conversation_input_activity_update BEFORE UPDATE ON conversation_activities
WHEN EXISTS(SELECT 1 FROM conversation_input_holds h JOIN conversation_input_custody c ON c.turn_id=h.turn_id
 JOIN conversation_input_actions a ON a.action_key=c.last_action_key
 WHERE h.turn_id IN (OLD.turn_id,NEW.turn_id) AND h.active=1 AND a.kind<>'delete')
BEGIN SELECT RAISE(ABORT,'INPUT_HELD'); END;
CREATE TRIGGER conversation_input_activity_delete BEFORE DELETE ON conversation_activities
WHEN EXISTS(SELECT 1 FROM conversation_input_holds h JOIN conversation_input_custody c ON c.turn_id=h.turn_id
 JOIN conversation_input_actions a ON a.action_key=c.last_action_key
 WHERE h.turn_id=OLD.turn_id AND h.active=1 AND a.kind<>'delete')
BEGIN SELECT RAISE(ABORT,'INPUT_HELD'); END;
CREATE TRIGGER conversation_input_activity_insert BEFORE INSERT ON conversation_activities
WHEN EXISTS(SELECT 1 FROM conversation_input_holds WHERE turn_id=NEW.turn_id AND active=1)
 OR EXISTS(SELECT 1 FROM conversation_activities a JOIN conversation_input_holds h ON h.turn_id=a.turn_id
  WHERE a.id=NEW.id AND h.active=1)
BEGIN SELECT RAISE(ABORT,'INPUT_HELD'); END;

CREATE TRIGGER conversation_input_action_apply AFTER INSERT ON conversation_input_actions
BEGIN
 -- Capture routing before delete removes the turn; a later failure rolls back CDC too.
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'conversationId',t.conversation_id,'inputCustodyChanged',json('true')),NEW.created_at
 FROM conversation_turns t JOIN sessions s ON s.id=t.handled_by_session_id WHERE t.id=NEW.turn_id;
 INSERT INTO conversation_input_custody(turn_id,owner,revision,original_state,original_conversation_id,original_branch_id,
  original_session_id,original_generation,original_sequence,requested_at,last_action_key)
 SELECT id,NEW.target_owner,1,state,conversation_id,branch_id,handled_by_session_id,controller_generation,intake_sequence,requested_at,NEW.action_key
 FROM conversation_turns WHERE id=NEW.turn_id AND NEW.kind='register';
 UPDATE conversation_input_custody SET revision=revision+1,last_action_key=NEW.action_key,
  owner=CASE WHEN NEW.kind='transfer' THEN NEW.target_owner ELSE owner END
 WHERE turn_id=NEW.turn_id AND NEW.kind<>'register';
 INSERT INTO conversation_input_holds(turn_id,hold_key,ledger_owner,reason,version,active,last_action_key)
 SELECT NEW.turn_id,NEW.hold_key,NEW.hold_owner,NEW.reason,NEW.hold_version+1,1,NEW.action_key WHERE NEW.kind='hold'
 ON CONFLICT(turn_id,hold_key) DO UPDATE SET ledger_owner=excluded.ledger_owner,reason=excluded.reason,version=excluded.version,active=1,last_action_key=excluded.last_action_key;
 UPDATE conversation_input_holds SET active=0,version=version+1,last_action_key=NEW.action_key
 WHERE turn_id=NEW.turn_id AND hold_key=NEW.hold_key AND NEW.kind='release';
 UPDATE conversation_turns SET state='held' WHERE id=NEW.turn_id AND NEW.kind='hold';
 UPDATE conversation_turns SET state=NEW.target_state WHERE id=NEW.turn_id AND NEW.kind='release'
  AND NOT EXISTS(SELECT 1 FROM conversation_input_holds WHERE turn_id=NEW.turn_id AND active=1);
 DELETE FROM conversation_messages WHERE turn_id=NEW.turn_id AND NEW.kind='delete';
 DELETE FROM conversation_activities WHERE turn_id=NEW.turn_id AND NEW.kind='delete';
 DELETE FROM conversation_input_holds WHERE turn_id=NEW.turn_id AND NEW.kind='delete';
 DELETE FROM conversation_turns WHERE id=NEW.turn_id AND NEW.kind='delete';
END;

CREATE VIEW orchestrator_replacement_held_turns AS
SELECT DISTINCT r.id AS operation_id, t.id AS turn_id
FROM conversation_turns t
JOIN conversations c ON c.id = t.conversation_id
JOIN conversation_messages m ON m.turn_id = t.id AND m.role = 'user'
JOIN orchestrator_replacements r ON r.project_id = c.project_id AND r.is_active = 1
WHERE (c.scope = 'project' OR (c.scope = 'session' AND (
 c.session_id = r.source_session_id
 OR c.session_id = json_extract(r.state_json, '$.successorSessionId')
 OR EXISTS (SELECT 1 FROM sessions s WHERE s.id = c.session_id AND s.kind = 'orchestrator'
  AND s.client_request_id <> ''
  AND s.client_request_id = json_extract(r.state_json, '$.spawnRequestId')
  AND s.client_request_hash = json_extract(r.state_json, '$.spawnRequestHash'))
)))
 AND t.state = 'queued'
 AND NOT EXISTS (SELECT 1 FROM orchestrator_replacement_released_sessions released
  WHERE released.operation_id = r.id AND released.session_id = c.current_session_id)
 AND NOT COALESCE((
  (r.phase = 'preparing_handoff'
   AND t.handled_by_session_id = r.source_session_id
   AND m.client_message_id <> ''
   AND m.client_message_id = json_extract(r.state_json, '$.preparation.controlClientId'))
  OR (r.phase IN ('starting', 'controller_ready')
   AND m.client_message_id <> ''
   AND m.client_message_id = json_extract(r.state_json, '$.bootstrapClientId')
   AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = t.handled_by_session_id
    AND s.client_request_id = json_extract(r.state_json, '$.spawnRequestId')
    AND s.client_request_hash = json_extract(r.state_json, '$.spawnRequestHash')))
  OR (r.phase = 'verifying_resume'
   AND t.handled_by_session_id = json_extract(r.state_json, '$.successorSessionId')
   AND m.client_message_id <> ''
   AND m.client_message_id = json_extract(r.state_json, '$.resumeProbeClientId'))
 ), 0)
UNION
-- Releasing ordinary intake must not dispatch a paused preparation control.
SELECT DISTINCT r.id AS operation_id, t.id AS turn_id
FROM orchestrator_replacements r
JOIN conversations c ON c.project_id = r.project_id
JOIN conversation_turns t ON t.conversation_id = c.id AND t.state = 'queued'
JOIN conversation_messages m ON m.turn_id = t.id AND m.role = 'user'
WHERE r.is_active = 1 AND r.phase IN ('awaiting_source', 'awaiting_handoff', 'awaiting_transcript')
 AND m.client_message_id <> ''
 AND m.client_message_id = json_extract(r.state_json, '$.preparation.controlClientId')
UNION
-- A late canonical receipt for a superseded preparation must never become
-- ordinary user intake, even after this operation is cancelled or completed.
SELECT DISTINCT r.id AS operation_id, t.id AS turn_id
FROM orchestrator_replacements r
JOIN conversations c ON c.project_id = r.project_id
JOIN conversation_turns t ON t.conversation_id = c.id AND t.state = 'queued'
JOIN conversation_messages m ON m.turn_id = t.id AND m.role = 'user'
JOIN json_each(r.state_json, '$.attempts') archived
WHERE m.client_message_id <> ''
 AND m.client_message_id = json_extract(archived.value, '$.preparation.controlClientId')
UNION
SELECT 'shared:' || h.hold_key AS operation_id, h.turn_id
FROM conversation_input_holds h WHERE h.active = 1;

CREATE VIEW orchestrator_replacement_released_sessions AS
SELECT r.id AS operation_id, s.id AS session_id
FROM orchestrator_replacements r
JOIN sessions s ON s.id = json_extract(r.state_json, '$.successorSessionId')
JOIN project_document_states d ON d.project_id = r.project_id
WHERE r.phase = 'notifying' AND r.is_active = 1
 AND json_type(r.state_json, '$.intakeReleasedAt') IS 'text'
 AND json_type(r.state_json, '$.writerActivation') IS 'object'
 AND s.project_id = r.project_id AND s.kind = 'orchestrator'
 AND s.harness = 'claude-code' AND s.session_mode = 'chat' AND s.is_terminated = 0
 AND s.controller_generation = json_extract(d.state_json, '$.lease.owner.ControllerGeneration')
 AND s.provider_conversation_id = json_extract(r.state_json, '$.writerActivation.owner.ProviderConversationID')
 AND (json_extract(d.state_json, '$.lease.claimVerifier') = s.browser_capability_verifier OR (COALESCE(json_extract(d.state_json, '$.lease.claimVerifier'), '') = '' AND COALESCE(json_extract(d.state_json, '$.lease.tokenHash'), '') <> ''))
 AND s.browser_capability_verifier = COALESCE(NULLIF(json_extract(d.state_json, '$.lease.capabilityVerifier'), ''),
   CASE WHEN json_extract(d.state_json, '$.lease.owner') = json_extract(r.state_json, '$.writerActivation.owner')
    THEN json_extract(r.state_json, '$.writerActivation.capabilityVerifier') END)
 AND json_extract(d.state_json, '$.lease.sessionId') = s.id
 AND json_extract(d.state_json, '$.lease.replacementId') = r.id
 AND json_extract(d.state_json, '$.lease.frozen') = 0
 AND json_extract(d.state_json, '$.lease.epoch') = json_extract(r.state_json, '$.writerActivation.afterEpoch')
 AND s.runtime_launch_id = json_extract(d.state_json, '$.lease.owner.RuntimeLaunchID')
 AND s.agent_session_id = json_extract(d.state_json, '$.lease.owner.AgentSessionID')
 AND s.agent_session_id_launch_id = json_extract(d.state_json, '$.lease.owner.AgentSessionIDLaunchID')
 AND json_extract(d.state_json, '$.lease.owner.ProviderConversationID') = s.provider_conversation_id AND json_extract(d.state_json, '$.lease.owner.IsTerminated') = 0 AND json_extract(d.state_json, '$.lease.owner.Harness') = s.harness AND json_extract(d.state_json, '$.lease.owner.Mode') = s.session_mode
UNION
-- A paused pre-stop attempt may return ordinary input to its original owner.
-- A later owner/lease rotation closes this release without trusting the marker.
SELECT r.id AS operation_id, s.id AS session_id
FROM orchestrator_replacements r
JOIN sessions s ON s.id = r.source_session_id
LEFT JOIN project_document_states d ON d.project_id = r.project_id
WHERE r.is_active = 1 AND r.phase IN ('awaiting_source', 'awaiting_handoff', 'awaiting_transcript')
 AND json_type(r.state_json, '$.sourceIntakeReleasedAt') IS 'text'
 AND COALESCE(json_extract(r.state_json, '$.successorSessionId'), '') = ''
 AND json_type(r.state_json, '$.sourceStopIntentAt') IS NULL
 AND json_type(r.state_json, '$.sourceStop') IS NULL
 AND json_type(r.state_json, '$.sourceRetiredAt') IS NULL
 AND s.project_id = r.project_id AND s.kind = 'orchestrator'
 AND s.is_terminated = 0 AND s.hibernated_at IS NULL AND s.activity_state <> 'exited'
 AND s.harness = json_extract(r.state_json, '$.sourceOwner.Harness')
 AND s.session_mode = json_extract(r.state_json, '$.sourceOwner.Mode')
 AND s.controller_generation = json_extract(r.state_json, '$.sourceOwner.ControllerGeneration')
 AND s.provider_conversation_id = json_extract(r.state_json, '$.sourceOwner.ProviderConversationID')
 AND s.runtime_launch_id = json_extract(r.state_json, '$.sourceOwner.RuntimeLaunchID')
 AND s.agent_session_id = json_extract(r.state_json, '$.sourceOwner.AgentSessionID')
 AND s.agent_session_id_launch_id = json_extract(r.state_json, '$.sourceOwner.AgentSessionIDLaunchID')
 AND s.browser_capability_verifier = json_extract(r.state_json, '$.sourceCapabilityVerifier')
 AND (d.project_id IS NULL OR json_type(d.state_json, '$.lease') IS NOT 'object'
  OR (json_extract(d.state_json, '$.lease.sessionId') = s.id
   AND json_extract(d.state_json, '$.lease.owner') = json_extract(r.state_json, '$.sourceOwner')
   AND json_extract(d.state_json, '$.lease.frozen') = 0
   AND COALESCE(json_extract(d.state_json, '$.lease.replacementId'), '') IN ('', r.id)
   AND (COALESCE(json_extract(r.state_json, '$.preparation.leaseEpoch'), 0) = 0
    OR json_extract(d.state_json, '$.lease.epoch') = json_extract(r.state_json, '$.preparation.leaseEpoch'))));

CREATE TRIGGER attention_turn_changed AFTER UPDATE OF state,rolled_back_at ON conversation_turns WHEN (OLD.state IS NOT NEW.state OR OLD.rolled_back_at IS NOT NEW.rolled_back_at) AND EXISTS(SELECT 1 FROM conversation_activities WHERE turn_id=NEW.id AND kind IN ('approval','user_input')) BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at) SELECT s.project_id,s.id,'session_updated',json_object('attentionChanged',json('true')),COALESCE(NEW.completed_at,NEW.started_at,NEW.requested_at) FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;

CREATE TRIGGER conversation_turn_settings_snapshot_cdc
AFTER UPDATE OF agent_settings_snapshot_json ON conversation_turns
WHEN OLD.agent_settings_snapshot_json <> NEW.agent_settings_snapshot_json
BEGIN
    INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
    SELECT s.project_id,s.id,'session_updated',
      json_object('id',s.id,'sessionId',s.id,'conversationId',NEW.conversation_id),CURRENT_TIMESTAMP
    FROM conversations c JOIN sessions s ON s.id=c.current_session_id
    WHERE c.id=NEW.conversation_id;
END;

CREATE TRIGGER conversation_turns_auto_resume_cdc_update
AFTER UPDATE OF usage_limit_resets_at, usage_limit_auto_resume, usage_limit_resume_error ON conversation_turns
WHEN OLD.usage_limit_resets_at IS NOT NEW.usage_limit_resets_at
  OR OLD.usage_limit_auto_resume <> NEW.usage_limit_auto_resume
  OR OLD.usage_limit_resume_error <> NEW.usage_limit_resume_error
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
        json_object('id', s.id, 'sessionId', s.id, 'conversationId', NEW.conversation_id), CURRENT_TIMESTAMP
    FROM conversations c JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;

CREATE TRIGGER conversation_turns_branch_insert AFTER INSERT ON conversation_turns
WHEN NEW.branch_id = '' BEGIN
  UPDATE conversation_turns SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id) WHERE id = NEW.id;
END;

CREATE TRIGGER conversation_turns_cdc_update AFTER UPDATE ON conversation_turns WHEN OLD.state <> NEW.state BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',NEW.handled_by_review_id,'conversationId',NEW.conversation_id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),COALESCE(NEW.completed_at,NEW.started_at,NEW.requested_at) FROM sessions s WHERE s.id=NEW.handled_by_session_id;
END;

CREATE TRIGGER replacement_intake_delete
BEFORE DELETE ON conversation_messages
WHEN ((OLD.role = 'user' AND OLD.origin IN ('human','automation','daemon')))
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id IN (SELECT project_id FROM conversations WHERE id IN (OLD.conversation_id)
  AND (scope = 'project' OR (scope = 'session' AND session_id = orchestrator_replacements.source_session_id)))
 AND (OLD.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '') OR OLD.client_message_id = '');
END;

CREATE TRIGGER replacement_intake_insert
AFTER INSERT ON conversation_messages
WHEN ((NEW.role = 'user' AND NEW.origin IN ('human','automation','daemon')))
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id IN (SELECT project_id FROM conversations WHERE id IN (NEW.conversation_id)
  AND (scope = 'project' OR (scope = 'session' AND session_id = orchestrator_replacements.source_session_id)))
 AND (NEW.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '') OR NEW.client_message_id = '');
END;

CREATE TRIGGER replacement_intake_turn_settled
AFTER UPDATE OF state ON conversation_turns
WHEN OLD.state = 'queued' AND NEW.state <> 'queued'
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND EXISTS (SELECT 1 FROM conversations c JOIN conversation_messages m
  ON m.conversation_id = c.id AND m.turn_id = NEW.id
  WHERE c.id = NEW.conversation_id
   AND c.project_id = orchestrator_replacements.project_id
   AND (c.scope = 'project' OR (c.scope = 'session' AND c.session_id = orchestrator_replacements.source_session_id))
   AND m.role = 'user' AND m.origin IN ('human','automation','daemon')
   AND (m.client_message_id = '' OR m.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '')));
END;

CREATE TRIGGER replacement_intake_update
AFTER UPDATE ON conversation_messages
WHEN ((OLD.role = 'user' AND OLD.origin IN ('human','automation','daemon')) OR (NEW.role = 'user' AND NEW.origin IN ('human','automation','daemon')))
 AND (OLD.text IS NOT NEW.text OR OLD.delivery_content_json IS NOT NEW.delivery_content_json OR OLD.client_payload_hash IS NOT NEW.client_payload_hash OR OLD.sender_session_id IS NOT NEW.sender_session_id OR OLD.sender_project_id IS NOT NEW.sender_project_id OR OLD.sender_display_name IS NOT NEW.sender_display_name OR OLD.client_message_id IS NOT NEW.client_message_id OR OLD.conversation_id IS NOT NEW.conversation_id OR OLD.turn_id IS NOT NEW.turn_id OR OLD.role IS NOT NEW.role OR OLD.origin IS NOT NEW.origin OR OLD.continuation IS NOT NEW.continuation)
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id IN (SELECT project_id FROM conversations WHERE id IN (OLD.conversation_id,NEW.conversation_id)
  AND (scope = 'project' OR (scope = 'session' AND session_id = orchestrator_replacements.source_session_id)))
 AND (OLD.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '') OR OLD.client_message_id = '' OR NEW.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '') OR NEW.client_message_id = '');
END;

CREATE TRIGGER replacement_notice_history_no_delete BEFORE DELETE ON orchestrator_replacement_notice_history
BEGIN SELECT RAISE(ABORT,'replacement notice history cannot be deleted'); END;

CREATE TRIGGER replacement_notice_history_no_update BEFORE UPDATE ON orchestrator_replacement_notice_history
BEGIN SELECT RAISE(ABORT,'replacement notice history is immutable'); END;

CREATE TRIGGER replacement_notice_identity BEFORE UPDATE ON orchestrator_replacement_notices
WHEN OLD.operation_id IS NOT NEW.operation_id OR OLD.session_id IS NOT NEW.session_id
 OR OLD.contact_id IS NOT NEW.contact_id OR OLD.state IN ('accepted','skipped')
BEGIN SELECT RAISE(ABORT,'replacement notice identity or settled receipt is immutable'); END;

CREATE TRIGGER replacement_notice_no_delete BEFORE DELETE ON orchestrator_replacement_notices
BEGIN SELECT RAISE(ABORT,'replacement notice receipts cannot be deleted'); END;

CREATE TRIGGER replacement_orchestrator_insert_fence
BEFORE INSERT ON sessions
WHEN NEW.kind = 'orchestrator' AND NEW.is_terminated = 0
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_IN_PROGRESS')
 WHERE EXISTS (
  SELECT 1 FROM orchestrator_replacements r
  WHERE r.project_id = NEW.project_id AND r.is_active = 1
   AND NOT COALESCE((
    r.phase = 'starting'
    AND json_type(r.state_json, '$.sourceRetiredAt') IS 'text'
    AND COALESCE(json_extract(r.state_json, '$.successorSessionId'), '') = ''
    AND COALESCE(json_extract(r.state_json, '$.spawnRequestId'), '') <> ''
    AND json_extract(r.state_json, '$.spawnRequestId') = NEW.client_request_id
    AND json_extract(r.state_json, '$.spawnRequestHash') = NEW.client_request_hash
    AND NEW.harness = 'claude-code' AND NEW.session_mode = 'chat'
    AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = r.source_session_id AND s.is_terminated = 1)
    AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.project_id = NEW.project_id AND s.kind = 'orchestrator' AND s.is_terminated = 0)
   ), 0)
 );
END;

CREATE TRIGGER replacement_orchestrator_restore_fence
BEFORE UPDATE OF kind, is_terminated, project_id ON sessions
WHEN NEW.is_terminated = 0
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_SOURCE_RETIRED')
 WHERE OLD.is_terminated = 1 AND EXISTS (
  SELECT 1 FROM orchestrator_replacements r WHERE r.source_session_id = NEW.id
   AND json_type(r.state_json, '$.sourceRetiredAt') IS 'text'
 );
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_IN_PROGRESS')
 WHERE NEW.kind = 'orchestrator'
  AND (OLD.is_terminated = 1 OR OLD.kind <> 'orchestrator' OR OLD.project_id IS NOT NEW.project_id)
  AND EXISTS (
   SELECT 1 FROM orchestrator_replacements r
   WHERE r.project_id = NEW.project_id AND r.is_active = 1
    AND NOT COALESCE((
     r.phase = 'starting'
     AND json_extract(r.state_json, '$.successorSessionId') = NEW.id
     AND json_extract(r.state_json, '$.spawnRequestId') = NEW.client_request_id
     AND json_extract(r.state_json, '$.spawnRequestHash') = NEW.client_request_hash
     AND NEW.harness = 'claude-code' AND NEW.session_mode = 'chat'
     AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = r.source_session_id AND s.is_terminated = 1)
     AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.project_id = NEW.project_id AND s.id <> NEW.id AND s.kind = 'orchestrator' AND s.is_terminated = 0)
    ), 0)
  );
END;

CREATE TRIGGER replacement_repair_snapshot_no_delete
BEFORE DELETE ON orchestrator_replacement_repair_snapshots
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPAIR_SNAPSHOT_IMMUTABLE');
END;

CREATE TRIGGER replacement_repair_snapshot_no_update
BEFORE UPDATE ON orchestrator_replacement_repair_snapshots
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPAIR_SNAPSHOT_IMMUTABLE');
END;

CREATE TRIGGER replacement_report_intake_delete
BEFORE DELETE ON reports
WHEN EXISTS (SELECT 1 FROM sessions s WHERE s.id = OLD.session_id AND s.project_id = OLD.project_id AND s.kind = 'worker')
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id = OLD.project_id;
END;

CREATE TRIGGER replacement_report_intake_insert
AFTER INSERT ON reports
WHEN EXISTS (SELECT 1 FROM sessions s WHERE s.id = NEW.session_id AND s.project_id = NEW.project_id AND s.kind = 'worker')
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id = NEW.project_id;
END;

CREATE TRIGGER replacement_report_intake_update
AFTER UPDATE ON reports
WHEN OLD.note IS NOT NEW.note OR OLD.message IS NOT NEW.message OR OLD.state IS NOT NEW.state
 OR OLD.session_id IS NOT NEW.session_id OR OLD.project_id IS NOT NEW.project_id
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL AND (
  (project_id = OLD.project_id AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = OLD.session_id AND s.project_id = OLD.project_id AND s.kind = 'worker'))
  OR (project_id = NEW.project_id AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = NEW.session_id AND s.project_id = NEW.project_id AND s.kind = 'worker'))
 );
END;

CREATE TRIGGER replacement_successor_bind
AFTER INSERT ON sessions
WHEN NEW.kind = 'orchestrator' AND NEW.is_terminated = 0
 AND NEW.harness = 'claude-code' AND NEW.session_mode = 'chat'
BEGIN
 UPDATE orchestrator_replacements
 SET state_json = json_set(state_json, '$.successorSessionId', NEW.id),
     revision = revision + 1
 WHERE project_id = NEW.project_id AND is_active = 1 AND phase = 'starting'
  AND json_type(state_json, '$.sourceRetiredAt') IS 'text'
  AND COALESCE(json_extract(state_json, '$.successorSessionId'), '') = ''
  AND COALESCE(json_extract(state_json, '$.spawnRequestId'), '') <> ''
  AND json_extract(state_json, '$.spawnRequestId') = NEW.client_request_id
  AND json_extract(state_json, '$.spawnRequestHash') = NEW.client_request_hash;
END;

CREATE TRIGGER replacement_successor_delete_fence
BEFORE DELETE ON sessions
WHEN EXISTS (
 SELECT 1 FROM orchestrator_replacements r WHERE r.is_active = 1
 AND json_extract(r.state_json, '$.successorSessionId') = OLD.id
)
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_SUCCESSOR_RETAINED');
END;

CREATE TRIGGER replacement_turn_dispatch_fence
BEFORE UPDATE OF state, promotion_started_at ON conversation_turns
WHEN OLD.state = 'queued'
 AND (NEW.state = 'running' OR (OLD.promotion_started_at IS NULL AND NEW.promotion_started_at IS NOT NULL))
 AND EXISTS (SELECT 1 FROM orchestrator_replacement_held_turns h WHERE h.turn_id = OLD.id)
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_INTAKE_FENCED');
END;
CREATE TEMP TABLE shared_input_fk_check(ok INTEGER CHECK(ok = 1));
DELETE FROM shared_input_fk_tables;
INSERT INTO shared_input_fk_tables
 SELECT name FROM main.sqlite_master m WHERE type='table' AND (
  name IN ('conversation_turns','conversation_input_custody','conversation_input_holds')
  OR EXISTS(SELECT 1 FROM pragma_foreign_key_list(m.name) fk WHERE fk."table"='conversation_turns')
 );
INSERT INTO shared_input_fk_check SELECT NOT EXISTS(
 SELECT fk."table",fk.rowid,fk.parent,fk.fkid
 FROM shared_input_fk_tables t JOIN pragma_foreign_key_check(t.name) fk
 EXCEPT SELECT * FROM shared_input_fk_before
);
DROP TABLE shared_input_fk_check;
DROP TABLE shared_input_fk_before;
DROP TABLE shared_input_fk_tables;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TEMP TABLE shared_input_fk_mode(ok INTEGER CHECK(ok=1));
INSERT INTO shared_input_fk_mode SELECT foreign_keys=0 FROM pragma_foreign_keys;
DROP TABLE shared_input_fk_mode;
-- Preserve legacy violations; inspect only the rebuilt table, its children,
-- and shared custody tables. Materialize names before scanning their rows.
CREATE TEMP TABLE shared_input_fk_tables(name TEXT PRIMARY KEY);
INSERT INTO shared_input_fk_tables
 SELECT name FROM main.sqlite_master m WHERE type='table' AND (
  name IN ('conversation_turns','conversation_input_custody','conversation_input_holds')
  OR EXISTS(SELECT 1 FROM pragma_foreign_key_list(m.name) fk WHERE fk."table"='conversation_turns')
 );
CREATE TEMP TABLE shared_input_fk_before("table" TEXT,rowid INTEGER,parent TEXT,fkid INTEGER);
INSERT INTO shared_input_fk_before
SELECT fk."table",fk.rowid,fk.parent,fk.fkid
 FROM shared_input_fk_tables t JOIN pragma_foreign_key_check(t.name) fk;
CREATE TEMP TABLE shared_input_down_check(ok INTEGER CHECK(ok = 1));
-- Binary backout keeps this schema; Down is only safe before its first use.
INSERT INTO shared_input_down_check SELECT NOT EXISTS(SELECT 1 FROM conversation_input_actions) AND NOT EXISTS(SELECT 1 FROM conversation_input_custody) AND NOT EXISTS(SELECT 1 FROM conversation_input_holds) AND NOT EXISTS(SELECT 1 FROM conversation_turns WHERE state = 'held');
DROP TABLE shared_input_down_check;
DROP TRIGGER attention_turn_changed;
DROP TRIGGER conversation_turn_settings_snapshot_cdc;
DROP TRIGGER conversation_turns_auto_resume_cdc_update;
DROP TRIGGER conversation_turns_branch_insert;
DROP TRIGGER conversation_turns_cdc_update;
DROP TRIGGER replacement_intake_delete;
DROP TRIGGER replacement_intake_insert;
DROP TRIGGER replacement_intake_turn_settled;
DROP TRIGGER replacement_intake_update;
DROP TRIGGER replacement_notice_history_no_delete;
DROP TRIGGER replacement_notice_history_no_update;
DROP TRIGGER replacement_notice_identity;
DROP TRIGGER replacement_notice_no_delete;
DROP TRIGGER replacement_orchestrator_insert_fence;
DROP TRIGGER replacement_orchestrator_restore_fence;
DROP TRIGGER replacement_repair_snapshot_no_delete;
DROP TRIGGER replacement_repair_snapshot_no_update;
DROP TRIGGER replacement_report_intake_delete;
DROP TRIGGER replacement_report_intake_insert;
DROP TRIGGER replacement_report_intake_update;
DROP TRIGGER replacement_successor_bind;
DROP TRIGGER replacement_successor_delete_fence;
DROP TRIGGER replacement_turn_dispatch_fence;
DROP VIEW orchestrator_replacement_held_turns;
DROP VIEW orchestrator_replacement_released_sessions;
DROP TRIGGER conversation_input_action_validate;
DROP TRIGGER conversation_input_action_no_update;
DROP TRIGGER conversation_input_action_no_delete;
DROP TRIGGER conversation_input_custody_insert;
DROP TRIGGER conversation_input_custody_update;
DROP TRIGGER conversation_input_custody_delete;
DROP TRIGGER conversation_input_hold_insert;
DROP TRIGGER conversation_input_hold_update;
DROP TRIGGER conversation_input_hold_delete;
DROP TRIGGER conversation_input_turn_insert;
DROP TRIGGER conversation_input_turn_update;
DROP TRIGGER conversation_input_turn_delete;
DROP TRIGGER conversation_input_message_insert;
DROP TRIGGER conversation_input_message_update;
DROP TRIGGER conversation_input_message_delete;
DROP TRIGGER conversation_input_activity_update;
DROP TRIGGER conversation_input_activity_delete;
DROP TRIGGER conversation_input_activity_insert;
DROP TRIGGER conversation_input_action_apply;
DROP TABLE conversation_input_holds;
DROP TABLE conversation_input_custody;
DROP TABLE conversation_input_actions;
CREATE TABLE conversation_turns_next (
    id                     TEXT PRIMARY KEY,
    conversation_id        TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    handled_by_session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    provider_turn_id       TEXT NOT NULL DEFAULT '',
    controller_generation  TEXT NOT NULL DEFAULT '',
    state                  TEXT NOT NULL CHECK (state IN ('queued', 'running', 'completed', 'recovered', 'interrupted', 'failed', 'cancelled')),
    error_message          TEXT NOT NULL DEFAULT '',
    requested_at           TIMESTAMP NOT NULL,
    started_at             TIMESTAMP,
    completed_at           TIMESTAMP,
    diff_json              TEXT NOT NULL DEFAULT '',
    rolled_back_at         TIMESTAMP,
    plan_json              TEXT NOT NULL DEFAULT '',
    branch_id              TEXT NOT NULL DEFAULT '',
    promotion_started_at   TIMESTAMP,
    promoted_to_turn_id    TEXT REFERENCES conversation_turns_next(id) ON DELETE SET NULL,
    retry_of_turn_id       TEXT REFERENCES conversation_turns_next(id) ON DELETE RESTRICT
, handled_by_review_id TEXT REFERENCES review(id) ON DELETE SET NULL, usage_limit_resets_at TIMESTAMP, usage_limit_auto_resume INTEGER NOT NULL DEFAULT 0 CHECK (usage_limit_auto_resume IN (0, 1)), usage_limit_resume_error TEXT NOT NULL DEFAULT '', batched_input INTEGER NOT NULL DEFAULT 0, provider_input_text TEXT NOT NULL DEFAULT '', intake_sequence INTEGER NOT NULL DEFAULT 0 CHECK(intake_sequence >= 0), agent_settings_snapshot_json TEXT NOT NULL DEFAULT ''
CHECK (agent_settings_snapshot_json='' OR json_valid(agent_settings_snapshot_json)));
INSERT INTO conversation_turns_next(rowid, id,conversation_id,handled_by_session_id,provider_turn_id,controller_generation,state,error_message,requested_at,started_at,completed_at,diff_json,rolled_back_at,plan_json,branch_id,promotion_started_at,promoted_to_turn_id,retry_of_turn_id,handled_by_review_id,usage_limit_resets_at,usage_limit_auto_resume,usage_limit_resume_error,batched_input,provider_input_text,intake_sequence,agent_settings_snapshot_json) SELECT rowid, id,conversation_id,handled_by_session_id,provider_turn_id,controller_generation,state,error_message,requested_at,started_at,completed_at,diff_json,rolled_back_at,plan_json,branch_id,promotion_started_at,promoted_to_turn_id,retry_of_turn_id,handled_by_review_id,usage_limit_resets_at,usage_limit_auto_resume,usage_limit_resume_error,batched_input,provider_input_text,intake_sequence,agent_settings_snapshot_json FROM conversation_turns ORDER BY rowid;
DROP TABLE conversation_turns;
ALTER TABLE conversation_turns_next RENAME TO conversation_turns;
CREATE INDEX conversation_turns_auto_resume ON conversation_turns(usage_limit_resets_at) WHERE usage_limit_auto_resume = 1;
CREATE INDEX idx_conversation_turns_branch
    ON conversation_turns(branch_id, requested_at);
CREATE INDEX idx_conversation_turns_conversation
    ON conversation_turns(conversation_id, requested_at);
CREATE UNIQUE INDEX idx_conversation_turns_provider
    ON conversation_turns(conversation_id, provider_turn_id)
    WHERE provider_turn_id <> '';
CREATE INDEX idx_conversation_turns_retry_source
    ON conversation_turns(conversation_id, retry_of_turn_id)
    WHERE retry_of_turn_id IS NOT NULL;
CREATE VIEW orchestrator_replacement_held_turns AS
SELECT DISTINCT r.id AS operation_id, t.id AS turn_id
FROM conversation_turns t
JOIN conversations c ON c.id = t.conversation_id
JOIN conversation_messages m ON m.turn_id = t.id AND m.role = 'user'
JOIN orchestrator_replacements r ON r.project_id = c.project_id AND r.is_active = 1
WHERE (c.scope = 'project' OR (c.scope = 'session' AND (
 c.session_id = r.source_session_id
 OR c.session_id = json_extract(r.state_json, '$.successorSessionId')
 OR EXISTS (SELECT 1 FROM sessions s WHERE s.id = c.session_id AND s.kind = 'orchestrator'
  AND s.client_request_id <> ''
  AND s.client_request_id = json_extract(r.state_json, '$.spawnRequestId')
  AND s.client_request_hash = json_extract(r.state_json, '$.spawnRequestHash'))
)))
 AND t.state = 'queued'
 AND NOT EXISTS (SELECT 1 FROM orchestrator_replacement_released_sessions released
  WHERE released.operation_id = r.id AND released.session_id = c.current_session_id)
 AND NOT COALESCE((
  (r.phase = 'preparing_handoff'
   AND t.handled_by_session_id = r.source_session_id
   AND m.client_message_id <> ''
   AND m.client_message_id = json_extract(r.state_json, '$.preparation.controlClientId'))
  OR (r.phase IN ('starting', 'controller_ready')
   AND m.client_message_id <> ''
   AND m.client_message_id = json_extract(r.state_json, '$.bootstrapClientId')
   AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = t.handled_by_session_id
    AND s.client_request_id = json_extract(r.state_json, '$.spawnRequestId')
    AND s.client_request_hash = json_extract(r.state_json, '$.spawnRequestHash')))
  OR (r.phase = 'verifying_resume'
   AND t.handled_by_session_id = json_extract(r.state_json, '$.successorSessionId')
   AND m.client_message_id <> ''
   AND m.client_message_id = json_extract(r.state_json, '$.resumeProbeClientId'))
 ), 0)
UNION
-- Releasing ordinary intake must not dispatch a paused preparation control.
SELECT DISTINCT r.id AS operation_id, t.id AS turn_id
FROM orchestrator_replacements r
JOIN conversations c ON c.project_id = r.project_id
JOIN conversation_turns t ON t.conversation_id = c.id AND t.state = 'queued'
JOIN conversation_messages m ON m.turn_id = t.id AND m.role = 'user'
WHERE r.is_active = 1 AND r.phase IN ('awaiting_source', 'awaiting_handoff', 'awaiting_transcript')
 AND m.client_message_id <> ''
 AND m.client_message_id = json_extract(r.state_json, '$.preparation.controlClientId')
UNION
-- A late canonical receipt for a superseded preparation must never become
-- ordinary user intake, even after this operation is cancelled or completed.
SELECT DISTINCT r.id AS operation_id, t.id AS turn_id
FROM orchestrator_replacements r
JOIN conversations c ON c.project_id = r.project_id
JOIN conversation_turns t ON t.conversation_id = c.id AND t.state = 'queued'
JOIN conversation_messages m ON m.turn_id = t.id AND m.role = 'user'
JOIN json_each(r.state_json, '$.attempts') archived
WHERE m.client_message_id <> ''
 AND m.client_message_id = json_extract(archived.value, '$.preparation.controlClientId');

CREATE VIEW orchestrator_replacement_released_sessions AS
SELECT r.id AS operation_id, s.id AS session_id
FROM orchestrator_replacements r
JOIN sessions s ON s.id = json_extract(r.state_json, '$.successorSessionId')
JOIN project_document_states d ON d.project_id = r.project_id
WHERE r.phase = 'notifying' AND r.is_active = 1
 AND json_type(r.state_json, '$.intakeReleasedAt') IS 'text'
 AND json_type(r.state_json, '$.writerActivation') IS 'object'
 AND s.project_id = r.project_id AND s.kind = 'orchestrator'
 AND s.harness = 'claude-code' AND s.session_mode = 'chat' AND s.is_terminated = 0
 AND s.controller_generation = json_extract(d.state_json, '$.lease.owner.ControllerGeneration')
 AND s.provider_conversation_id = json_extract(r.state_json, '$.writerActivation.owner.ProviderConversationID')
 AND (json_extract(d.state_json, '$.lease.claimVerifier') = s.browser_capability_verifier OR (COALESCE(json_extract(d.state_json, '$.lease.claimVerifier'), '') = '' AND COALESCE(json_extract(d.state_json, '$.lease.tokenHash'), '') <> ''))
 AND s.browser_capability_verifier = COALESCE(NULLIF(json_extract(d.state_json, '$.lease.capabilityVerifier'), ''),
   CASE WHEN json_extract(d.state_json, '$.lease.owner') = json_extract(r.state_json, '$.writerActivation.owner')
    THEN json_extract(r.state_json, '$.writerActivation.capabilityVerifier') END)
 AND json_extract(d.state_json, '$.lease.sessionId') = s.id
 AND json_extract(d.state_json, '$.lease.replacementId') = r.id
 AND json_extract(d.state_json, '$.lease.frozen') = 0
 AND json_extract(d.state_json, '$.lease.epoch') = json_extract(r.state_json, '$.writerActivation.afterEpoch')
 AND s.runtime_launch_id = json_extract(d.state_json, '$.lease.owner.RuntimeLaunchID')
 AND s.agent_session_id = json_extract(d.state_json, '$.lease.owner.AgentSessionID')
 AND s.agent_session_id_launch_id = json_extract(d.state_json, '$.lease.owner.AgentSessionIDLaunchID')
 AND json_extract(d.state_json, '$.lease.owner.ProviderConversationID') = s.provider_conversation_id AND json_extract(d.state_json, '$.lease.owner.IsTerminated') = 0 AND json_extract(d.state_json, '$.lease.owner.Harness') = s.harness AND json_extract(d.state_json, '$.lease.owner.Mode') = s.session_mode
UNION
-- A paused pre-stop attempt may return ordinary input to its original owner.
-- A later owner/lease rotation closes this release without trusting the marker.
SELECT r.id AS operation_id, s.id AS session_id
FROM orchestrator_replacements r
JOIN sessions s ON s.id = r.source_session_id
LEFT JOIN project_document_states d ON d.project_id = r.project_id
WHERE r.is_active = 1 AND r.phase IN ('awaiting_source', 'awaiting_handoff', 'awaiting_transcript')
 AND json_type(r.state_json, '$.sourceIntakeReleasedAt') IS 'text'
 AND COALESCE(json_extract(r.state_json, '$.successorSessionId'), '') = ''
 AND json_type(r.state_json, '$.sourceStopIntentAt') IS NULL
 AND json_type(r.state_json, '$.sourceStop') IS NULL
 AND json_type(r.state_json, '$.sourceRetiredAt') IS NULL
 AND s.project_id = r.project_id AND s.kind = 'orchestrator'
 AND s.is_terminated = 0 AND s.hibernated_at IS NULL AND s.activity_state <> 'exited'
 AND s.harness = json_extract(r.state_json, '$.sourceOwner.Harness')
 AND s.session_mode = json_extract(r.state_json, '$.sourceOwner.Mode')
 AND s.controller_generation = json_extract(r.state_json, '$.sourceOwner.ControllerGeneration')
 AND s.provider_conversation_id = json_extract(r.state_json, '$.sourceOwner.ProviderConversationID')
 AND s.runtime_launch_id = json_extract(r.state_json, '$.sourceOwner.RuntimeLaunchID')
 AND s.agent_session_id = json_extract(r.state_json, '$.sourceOwner.AgentSessionID')
 AND s.agent_session_id_launch_id = json_extract(r.state_json, '$.sourceOwner.AgentSessionIDLaunchID')
 AND s.browser_capability_verifier = json_extract(r.state_json, '$.sourceCapabilityVerifier')
 AND (d.project_id IS NULL OR json_type(d.state_json, '$.lease') IS NOT 'object'
  OR (json_extract(d.state_json, '$.lease.sessionId') = s.id
   AND json_extract(d.state_json, '$.lease.owner') = json_extract(r.state_json, '$.sourceOwner')
   AND json_extract(d.state_json, '$.lease.frozen') = 0
   AND COALESCE(json_extract(d.state_json, '$.lease.replacementId'), '') IN ('', r.id)
   AND (COALESCE(json_extract(r.state_json, '$.preparation.leaseEpoch'), 0) = 0
    OR json_extract(d.state_json, '$.lease.epoch') = json_extract(r.state_json, '$.preparation.leaseEpoch'))));

CREATE TRIGGER attention_turn_changed AFTER UPDATE OF state,rolled_back_at ON conversation_turns WHEN (OLD.state IS NOT NEW.state OR OLD.rolled_back_at IS NOT NEW.rolled_back_at) AND EXISTS(SELECT 1 FROM conversation_activities WHERE turn_id=NEW.id AND kind IN ('approval','user_input')) BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at) SELECT s.project_id,s.id,'session_updated',json_object('attentionChanged',json('true')),COALESCE(NEW.completed_at,NEW.started_at,NEW.requested_at) FROM conversations c JOIN sessions s ON s.id=c.current_session_id WHERE c.id=NEW.conversation_id;
END;

CREATE TRIGGER conversation_turn_settings_snapshot_cdc
AFTER UPDATE OF agent_settings_snapshot_json ON conversation_turns
WHEN OLD.agent_settings_snapshot_json <> NEW.agent_settings_snapshot_json
BEGIN
    INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
    SELECT s.project_id,s.id,'session_updated',
      json_object('id',s.id,'sessionId',s.id,'conversationId',NEW.conversation_id),CURRENT_TIMESTAMP
    FROM conversations c JOIN sessions s ON s.id=c.current_session_id
    WHERE c.id=NEW.conversation_id;
END;

CREATE TRIGGER conversation_turns_auto_resume_cdc_update
AFTER UPDATE OF usage_limit_resets_at, usage_limit_auto_resume, usage_limit_resume_error ON conversation_turns
WHEN OLD.usage_limit_resets_at IS NOT NEW.usage_limit_resets_at
  OR OLD.usage_limit_auto_resume <> NEW.usage_limit_auto_resume
  OR OLD.usage_limit_resume_error <> NEW.usage_limit_resume_error
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
        json_object('id', s.id, 'sessionId', s.id, 'conversationId', NEW.conversation_id), CURRENT_TIMESTAMP
    FROM conversations c JOIN sessions s ON s.id = c.current_session_id
    WHERE c.id = NEW.conversation_id;
END;

CREATE TRIGGER conversation_turns_branch_insert AFTER INSERT ON conversation_turns
WHEN NEW.branch_id = '' BEGIN
  UPDATE conversation_turns SET branch_id = (SELECT active_branch_id FROM conversations WHERE id = NEW.conversation_id) WHERE id = NEW.id;
END;

CREATE TRIGGER conversation_turns_cdc_update AFTER UPDATE ON conversation_turns WHEN OLD.state <> NEW.state BEGIN
  INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
  SELECT s.project_id,s.id,'session_updated',json_object('id',s.id,'sessionId',s.id,'reviewId',NEW.handled_by_review_id,'conversationId',NEW.conversation_id,'activity',s.activity_state,'isTerminated',json(CASE WHEN s.is_terminated THEN 'true' ELSE 'false' END)),COALESCE(NEW.completed_at,NEW.started_at,NEW.requested_at) FROM sessions s WHERE s.id=NEW.handled_by_session_id;
END;

CREATE TRIGGER replacement_intake_delete
BEFORE DELETE ON conversation_messages
WHEN ((OLD.role = 'user' AND OLD.origin IN ('human','automation','daemon')))
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id IN (SELECT project_id FROM conversations WHERE id IN (OLD.conversation_id)
  AND (scope = 'project' OR (scope = 'session' AND session_id = orchestrator_replacements.source_session_id)))
 AND (OLD.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '') OR OLD.client_message_id = '');
END;

CREATE TRIGGER replacement_intake_insert
AFTER INSERT ON conversation_messages
WHEN ((NEW.role = 'user' AND NEW.origin IN ('human','automation','daemon')))
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id IN (SELECT project_id FROM conversations WHERE id IN (NEW.conversation_id)
  AND (scope = 'project' OR (scope = 'session' AND session_id = orchestrator_replacements.source_session_id)))
 AND (NEW.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '') OR NEW.client_message_id = '');
END;

CREATE TRIGGER replacement_intake_turn_settled
AFTER UPDATE OF state ON conversation_turns
WHEN OLD.state = 'queued' AND NEW.state <> 'queued'
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND EXISTS (SELECT 1 FROM conversations c JOIN conversation_messages m
  ON m.conversation_id = c.id AND m.turn_id = NEW.id
  WHERE c.id = NEW.conversation_id
   AND c.project_id = orchestrator_replacements.project_id
   AND (c.scope = 'project' OR (c.scope = 'session' AND c.session_id = orchestrator_replacements.source_session_id))
   AND m.role = 'user' AND m.origin IN ('human','automation','daemon')
   AND (m.client_message_id = '' OR m.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '')));
END;

CREATE TRIGGER replacement_intake_update
AFTER UPDATE ON conversation_messages
WHEN ((OLD.role = 'user' AND OLD.origin IN ('human','automation','daemon')) OR (NEW.role = 'user' AND NEW.origin IN ('human','automation','daemon')))
 AND (OLD.text IS NOT NEW.text OR OLD.delivery_content_json IS NOT NEW.delivery_content_json OR OLD.client_payload_hash IS NOT NEW.client_payload_hash OR OLD.sender_session_id IS NOT NEW.sender_session_id OR OLD.sender_project_id IS NOT NEW.sender_project_id OR OLD.sender_display_name IS NOT NEW.sender_display_name OR OLD.client_message_id IS NOT NEW.client_message_id OR OLD.conversation_id IS NOT NEW.conversation_id OR OLD.turn_id IS NOT NEW.turn_id OR OLD.role IS NOT NEW.role OR OLD.origin IS NOT NEW.origin OR OLD.continuation IS NOT NEW.continuation)
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id IN (SELECT project_id FROM conversations WHERE id IN (OLD.conversation_id,NEW.conversation_id)
  AND (scope = 'project' OR (scope = 'session' AND session_id = orchestrator_replacements.source_session_id)))
 AND (OLD.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '') OR OLD.client_message_id = '' OR NEW.client_message_id <> COALESCE(json_extract(state_json, '$.preparation.controlClientId'), '') OR NEW.client_message_id = '');
END;

CREATE TRIGGER replacement_notice_history_no_delete BEFORE DELETE ON orchestrator_replacement_notice_history
BEGIN SELECT RAISE(ABORT,'replacement notice history cannot be deleted'); END;

CREATE TRIGGER replacement_notice_history_no_update BEFORE UPDATE ON orchestrator_replacement_notice_history
BEGIN SELECT RAISE(ABORT,'replacement notice history is immutable'); END;

CREATE TRIGGER replacement_notice_identity BEFORE UPDATE ON orchestrator_replacement_notices
WHEN OLD.operation_id IS NOT NEW.operation_id OR OLD.session_id IS NOT NEW.session_id
 OR OLD.contact_id IS NOT NEW.contact_id OR OLD.state IN ('accepted','skipped')
BEGIN SELECT RAISE(ABORT,'replacement notice identity or settled receipt is immutable'); END;

CREATE TRIGGER replacement_notice_no_delete BEFORE DELETE ON orchestrator_replacement_notices
BEGIN SELECT RAISE(ABORT,'replacement notice receipts cannot be deleted'); END;

CREATE TRIGGER replacement_orchestrator_insert_fence
BEFORE INSERT ON sessions
WHEN NEW.kind = 'orchestrator' AND NEW.is_terminated = 0
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_IN_PROGRESS')
 WHERE EXISTS (
  SELECT 1 FROM orchestrator_replacements r
  WHERE r.project_id = NEW.project_id AND r.is_active = 1
   AND NOT COALESCE((
    r.phase = 'starting'
    AND json_type(r.state_json, '$.sourceRetiredAt') IS 'text'
    AND COALESCE(json_extract(r.state_json, '$.successorSessionId'), '') = ''
    AND COALESCE(json_extract(r.state_json, '$.spawnRequestId'), '') <> ''
    AND json_extract(r.state_json, '$.spawnRequestId') = NEW.client_request_id
    AND json_extract(r.state_json, '$.spawnRequestHash') = NEW.client_request_hash
    AND NEW.harness = 'claude-code' AND NEW.session_mode = 'chat'
    AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = r.source_session_id AND s.is_terminated = 1)
    AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.project_id = NEW.project_id AND s.kind = 'orchestrator' AND s.is_terminated = 0)
   ), 0)
 );
END;

CREATE TRIGGER replacement_orchestrator_restore_fence
BEFORE UPDATE OF kind, is_terminated, project_id ON sessions
WHEN NEW.is_terminated = 0
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_SOURCE_RETIRED')
 WHERE OLD.is_terminated = 1 AND EXISTS (
  SELECT 1 FROM orchestrator_replacements r WHERE r.source_session_id = NEW.id
   AND json_type(r.state_json, '$.sourceRetiredAt') IS 'text'
 );
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_IN_PROGRESS')
 WHERE NEW.kind = 'orchestrator'
  AND (OLD.is_terminated = 1 OR OLD.kind <> 'orchestrator' OR OLD.project_id IS NOT NEW.project_id)
  AND EXISTS (
   SELECT 1 FROM orchestrator_replacements r
   WHERE r.project_id = NEW.project_id AND r.is_active = 1
    AND NOT COALESCE((
     r.phase = 'starting'
     AND json_extract(r.state_json, '$.successorSessionId') = NEW.id
     AND json_extract(r.state_json, '$.spawnRequestId') = NEW.client_request_id
     AND json_extract(r.state_json, '$.spawnRequestHash') = NEW.client_request_hash
     AND NEW.harness = 'claude-code' AND NEW.session_mode = 'chat'
     AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = r.source_session_id AND s.is_terminated = 1)
     AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.project_id = NEW.project_id AND s.id <> NEW.id AND s.kind = 'orchestrator' AND s.is_terminated = 0)
    ), 0)
  );
END;

CREATE TRIGGER replacement_repair_snapshot_no_delete
BEFORE DELETE ON orchestrator_replacement_repair_snapshots
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPAIR_SNAPSHOT_IMMUTABLE');
END;

CREATE TRIGGER replacement_repair_snapshot_no_update
BEFORE UPDATE ON orchestrator_replacement_repair_snapshots
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPAIR_SNAPSHOT_IMMUTABLE');
END;

CREATE TRIGGER replacement_report_intake_delete
BEFORE DELETE ON reports
WHEN EXISTS (SELECT 1 FROM sessions s WHERE s.id = OLD.session_id AND s.project_id = OLD.project_id AND s.kind = 'worker')
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id = OLD.project_id;
END;

CREATE TRIGGER replacement_report_intake_insert
AFTER INSERT ON reports
WHEN EXISTS (SELECT 1 FROM sessions s WHERE s.id = NEW.session_id AND s.project_id = NEW.project_id AND s.kind = 'worker')
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id = NEW.project_id;
END;

CREATE TRIGGER replacement_report_intake_update
AFTER UPDATE ON reports
WHEN OLD.note IS NOT NEW.note OR OLD.message IS NOT NEW.message OR OLD.state IS NOT NEW.state
 OR OLD.session_id IS NOT NEW.session_id OR OLD.project_id IS NOT NEW.project_id
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL AND (
  (project_id = OLD.project_id AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = OLD.session_id AND s.project_id = OLD.project_id AND s.kind = 'worker'))
  OR (project_id = NEW.project_id AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = NEW.session_id AND s.project_id = NEW.project_id AND s.kind = 'worker'))
 );
END;

CREATE TRIGGER replacement_successor_bind
AFTER INSERT ON sessions
WHEN NEW.kind = 'orchestrator' AND NEW.is_terminated = 0
 AND NEW.harness = 'claude-code' AND NEW.session_mode = 'chat'
BEGIN
 UPDATE orchestrator_replacements
 SET state_json = json_set(state_json, '$.successorSessionId', NEW.id),
     revision = revision + 1
 WHERE project_id = NEW.project_id AND is_active = 1 AND phase = 'starting'
  AND json_type(state_json, '$.sourceRetiredAt') IS 'text'
  AND COALESCE(json_extract(state_json, '$.successorSessionId'), '') = ''
  AND COALESCE(json_extract(state_json, '$.spawnRequestId'), '') <> ''
  AND json_extract(state_json, '$.spawnRequestId') = NEW.client_request_id
  AND json_extract(state_json, '$.spawnRequestHash') = NEW.client_request_hash;
END;

CREATE TRIGGER replacement_successor_delete_fence
BEFORE DELETE ON sessions
WHEN EXISTS (
 SELECT 1 FROM orchestrator_replacements r WHERE r.is_active = 1
 AND json_extract(r.state_json, '$.successorSessionId') = OLD.id
)
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_SUCCESSOR_RETAINED');
END;

CREATE TRIGGER replacement_turn_dispatch_fence
BEFORE UPDATE OF state, promotion_started_at ON conversation_turns
WHEN OLD.state = 'queued'
 AND (NEW.state = 'running' OR (OLD.promotion_started_at IS NULL AND NEW.promotion_started_at IS NOT NULL))
 AND EXISTS (SELECT 1 FROM orchestrator_replacement_held_turns h WHERE h.turn_id = OLD.id)
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_INTAKE_FENCED');
END;
CREATE TEMP TABLE shared_input_fk_check(ok INTEGER CHECK(ok = 1));
DELETE FROM shared_input_fk_tables;
INSERT INTO shared_input_fk_tables
 SELECT name FROM main.sqlite_master m WHERE type='table' AND (
  name IN ('conversation_turns','conversation_input_custody','conversation_input_holds')
  OR EXISTS(SELECT 1 FROM pragma_foreign_key_list(m.name) fk WHERE fk."table"='conversation_turns')
 );
INSERT INTO shared_input_fk_check SELECT NOT EXISTS(
 SELECT fk."table",fk.rowid,fk.parent,fk.fkid
 FROM shared_input_fk_tables t JOIN pragma_foreign_key_check(t.name) fk
 EXCEPT SELECT * FROM shared_input_fk_before
);
DROP TABLE shared_input_fk_check;
DROP TABLE shared_input_fk_before;
DROP TABLE shared_input_fk_tables;
-- +goose StatementEnd
