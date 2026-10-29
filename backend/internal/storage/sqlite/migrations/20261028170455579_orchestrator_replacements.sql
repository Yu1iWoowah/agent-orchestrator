-- +goose Up
ALTER TABLE conversation_turns ADD COLUMN intake_sequence INTEGER NOT NULL DEFAULT 0 CHECK(intake_sequence >= 0);
UPDATE conversation_turns
SET intake_sequence = COALESCE((
 SELECT MIN(sequence) FROM (
  SELECT m.sequence FROM conversation_messages m WHERE m.turn_id = conversation_turns.id
  UNION ALL
  SELECT a.sequence FROM conversation_activities a WHERE a.turn_id = conversation_turns.id
 )
), 0);

CREATE TABLE orchestrator_replacements (
 id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 source_session_id TEXT NOT NULL,
 operation_key TEXT NOT NULL UNIQUE,
 request_hash TEXT NOT NULL,
 phase TEXT NOT NULL CHECK(phase IN (
  'accepted', 'preflight', 'waiting_source_idle', 'adopting_shared_files',
  'preparing_handoff', 'handoff_verified', 'freezing_shared_writes',
  'exporting_transcript', 'transcript_verified', 'retiring', 'starting',
  'controller_ready', 'handoff_settled', 'verifying_resume',
  'activating_shared_writer', 'notifying', 'completed', 'awaiting_source',
  'awaiting_handoff', 'awaiting_transcript', 'recovery_required', 'cancelled', 'rejected'
 )),
 is_active BOOLEAN NOT NULL CHECK(is_active = (phase NOT IN ('completed', 'cancelled', 'rejected'))),
 revision INTEGER NOT NULL CHECK(revision > 0),
 intake_revision INTEGER NOT NULL DEFAULT 0 CHECK(intake_revision >= 0),
 state_json TEXT NOT NULL CHECK(json_valid(state_json)),
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 CHECK(json_type(state_json, '$.id') IS 'text' AND json_extract(state_json, '$.id') = id),
 CHECK(json_type(state_json, '$.projectId') IS 'text' AND json_extract(state_json, '$.projectId') = project_id),
 CHECK(json_type(state_json, '$.sourceSessionId') IS 'text' AND json_extract(state_json, '$.sourceSessionId') = source_session_id),
 CHECK(json_type(state_json, '$.operationKey') IS 'text' AND json_extract(state_json, '$.operationKey') = operation_key),
 CHECK(json_type(state_json, '$.requestHash') IS 'text' AND json_extract(state_json, '$.requestHash') = request_hash),
 CHECK(json_type(state_json, '$.phase') IS 'text' AND json_extract(state_json, '$.phase') = phase),
 CHECK(json_type(state_json, '$.schemaVersion') IS 'integer' AND json_extract(state_json, '$.schemaVersion') = 1)
);

-- Original queue positions survive relocation and later edits. Do not attach
-- message/session foreign keys: a historical transfer receipt is not ownership
-- of the user's later retention/deletion decisions.
CREATE TABLE orchestrator_replacement_intake_transfers (
 operation_id TEXT NOT NULL REFERENCES orchestrator_replacements(id) ON DELETE CASCADE,
 message_id TEXT NOT NULL,
 turn_id TEXT NOT NULL,
 source_conversation_id TEXT NOT NULL,
 source_sequence INTEGER NOT NULL CHECK(source_sequence > 0),
 source_session_id TEXT NOT NULL,
 source_controller_generation TEXT NOT NULL,
 source_branch_id TEXT NOT NULL,
 target_conversation_id TEXT NOT NULL,
 target_sequence INTEGER NOT NULL CHECK(target_sequence > 0),
 payload_hash TEXT NOT NULL CHECK(length(payload_hash) = 64),
 transferred_at TIMESTAMP NOT NULL,
 PRIMARY KEY(operation_id, message_id),
 UNIQUE(operation_id, turn_id)
);
CREATE INDEX replacement_intake_transfer_source
ON orchestrator_replacement_intake_transfers(turn_id, target_conversation_id, source_conversation_id, payload_hash);

CREATE UNIQUE INDEX orchestrator_replacement_active_project
ON orchestrator_replacements(project_id) WHERE is_active = 1;

-- A final database fence covers other connections and legacy create/restore
-- paths. Ordinary updates to the current owner and all worker writes continue.
-- +goose StatementBegin
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
-- +goose StatementEnd

-- Bind the successor inside the session INSERT itself. A caller can disappear
-- immediately after creation without leaving an unbound, replaceable seed.
-- +goose StatementBegin
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
-- +goose StatementEnd

-- Failed provisioning retains this identity for explicit recovery. Generic
-- rollback must not erase the request key and make another successor possible.
-- +goose StatementBegin
CREATE TRIGGER replacement_successor_delete_fence
BEFORE DELETE ON sessions
WHEN EXISTS (
 SELECT 1 FROM orchestrator_replacements r WHERE r.is_active = 1
 AND json_extract(r.state_json, '$.successorSessionId') = OLD.id
)
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_SUCCESSOR_RETAINED');
END;
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- The existing queue remains authoritative for receipt IDs, attachments and
-- order. Only server-bound control IDs may cross an active replacement fence.
-- A release receipt only authorizes the exact verified successor writer.
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
 AND s.controller_generation = json_extract(r.state_json, '$.writerActivation.owner.ControllerGeneration')
 AND s.provider_conversation_id = json_extract(r.state_json, '$.writerActivation.owner.ProviderConversationID')
 AND s.browser_capability_verifier = json_extract(r.state_json, '$.writerActivation.capabilityVerifier')
 AND json_extract(d.state_json, '$.lease.sessionId') = s.id
 AND json_extract(d.state_json, '$.lease.replacementId') = r.id
 AND json_extract(d.state_json, '$.lease.frozen') = 0
 AND json_extract(d.state_json, '$.lease.epoch') = json_extract(r.state_json, '$.writerActivation.afterEpoch')
 AND json_extract(d.state_json, '$.lease.owner') = json_extract(r.state_json, '$.writerActivation.owner')
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

-- +goose StatementBegin
CREATE TRIGGER replacement_turn_dispatch_fence
BEFORE UPDATE OF state, promotion_started_at ON conversation_turns
WHEN OLD.state = 'queued'
 AND (NEW.state = 'running' OR (OLD.promotion_started_at IS NULL AND NEW.promotion_started_at IS NOT NULL))
 AND EXISTS (SELECT 1 FROM orchestrator_replacement_held_turns h WHERE h.turn_id = OLD.id)
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPLACEMENT_INTAKE_FENCED');
END;
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- Queue settlement changes the pending instruction set even when message bytes
-- remain unchanged. The daemon's owned preparation turn is control, not intake.
-- +goose StatementBegin
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
-- +goose StatementEnd

-- Pending worker reports remain in their durable outbox while preparation is
-- fenced. Their content is new intake even before native delivery; delivery
-- claims/ACKs do not change what section B must retain.
-- +goose StatementBegin
CREATE TRIGGER replacement_report_intake_insert
AFTER INSERT ON reports
WHEN EXISTS (SELECT 1 FROM sessions s WHERE s.id = NEW.session_id AND s.project_id = NEW.project_id AND s.kind = 'worker')
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id = NEW.project_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER replacement_report_intake_delete
BEFORE DELETE ON reports
WHEN EXISTS (SELECT 1 FROM sessions s WHERE s.id = OLD.session_id AND s.project_id = OLD.project_id AND s.kind = 'worker')
BEGIN
 UPDATE orchestrator_replacements SET intake_revision = intake_revision + 1
 WHERE is_active = 1 AND json_type(state_json, '$.frozenAt') IS NULL
 AND project_id = OLD.project_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER replacement_report_intake_delete;
DROP TRIGGER replacement_report_intake_update;
DROP TRIGGER replacement_report_intake_insert;
DROP TRIGGER replacement_intake_turn_settled;
DROP TRIGGER replacement_intake_delete;
DROP TRIGGER replacement_intake_update;
DROP TRIGGER replacement_intake_insert;
DROP TRIGGER replacement_turn_dispatch_fence;
DROP VIEW orchestrator_replacement_held_turns;
DROP VIEW orchestrator_replacement_released_sessions;
DROP TRIGGER replacement_orchestrator_restore_fence;
DROP TRIGGER replacement_successor_delete_fence;
DROP TRIGGER replacement_successor_bind;
DROP TRIGGER replacement_orchestrator_insert_fence;
DROP TABLE orchestrator_replacement_intake_transfers;
DROP TABLE orchestrator_replacements;
ALTER TABLE conversation_turns DROP COLUMN intake_sequence;
