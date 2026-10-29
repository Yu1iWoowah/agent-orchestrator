-- name: GetOrchestratorReplacement :one
SELECT * FROM orchestrator_replacements WHERE id = ?;

-- Bounded nonce acknowledgments only; never a transcript-preview truncation.
-- Keep limits aligned with domain.MaxReplacementProbeReplyBytes/Messages.
-- name: SelectReplacementProbeReplies :many
SELECT id, CAST(substr(CAST(text AS BLOB), 1, 65537) AS TEXT) AS bounded_text,
    CAST(length(CAST(text AS BLOB)) AS INTEGER) AS byte_length
FROM conversation_messages
WHERE conversation_id = sqlc.arg(conversation_id)
  AND turn_id = sqlc.arg(turn_id) AND role = 'assistant'
ORDER BY sequence LIMIT 65;

-- name: GetOrchestratorReplacementByKey :one
SELECT * FROM orchestrator_replacements WHERE operation_key = ?;

-- name: GetActiveOrchestratorReplacement :one
SELECT * FROM orchestrator_replacements WHERE project_id = ? AND is_active = 1;

-- name: ListActiveOrchestratorReplacements :many
SELECT * FROM orchestrator_replacements WHERE is_active = 1 ORDER BY created_at, id;

-- name: InsertOrchestratorReplacement :exec
INSERT INTO orchestrator_replacements(
 id, project_id, source_session_id, operation_key, request_hash, phase,
 is_active, revision, state_json, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?);

-- name: UpdateOrchestratorReplacement :execrows
UPDATE orchestrator_replacements
SET phase = ?, is_active = ?, revision = revision + 1, state_json = ?, updated_at = ?
WHERE id = ? AND revision = ?;

-- name: GetRetiredOrchestratorReplacementBySource :one
SELECT * FROM orchestrator_replacements
WHERE source_session_id = sqlc.arg(source_session_id)
 AND json_type(state_json, '$.sourceRetiredAt') IS 'text'
ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: RetireReplacementSession :execrows
-- Called only inside the verified replacement writer reservation. Preserve
-- workspace, branch, native history, capability and all worker facts.
UPDATE sessions SET is_terminated = 1, activity_state = 'exited',
 activity_last_at = sqlc.arg(retired_at), updated_at = sqlc.arg(retired_at)
WHERE id = sqlc.arg(session_id) AND is_terminated = 0 AND revision = sqlc.arg(expected_revision);

-- name: ReplacementWakeFenced :one
SELECT EXISTS (
 SELECT 1 FROM sessions s JOIN orchestrator_replacements r ON r.project_id = s.project_id
 WHERE s.id = sqlc.arg(session_id) AND s.kind = 'orchestrator'
 AND ((r.source_session_id = s.id AND json_type(r.state_json, '$.sourceRetiredAt') IS 'text')
 OR (r.is_active = 1 AND NOT EXISTS (SELECT 1 FROM orchestrator_replacement_released_sessions released
  WHERE released.operation_id = r.id AND released.session_id = s.id)))
);

-- Live pending intake, deliberately independent of frozen transcript cutoffs.
-- The existing queue owns payload/receipt identity; this query never reserves it.
-- name: SelectReplacementPendingMessages :many
SELECT m.id, m.conversation_id, m.turn_id, m.sequence, m.client_message_id,
 m.text, m.delivery_content_json, m.origin,
 m.sender_session_id, m.sender_project_id, m.sender_display_name,
 t.requested_at, t.provider_turn_id, t.promotion_started_at
FROM orchestrator_replacements r
JOIN conversations c ON c.project_id = r.project_id
JOIN conversation_turns t ON t.conversation_id = c.id
JOIN conversation_messages m ON m.turn_id = t.id AND m.conversation_id = c.id
WHERE r.id = sqlc.arg(operation_id) AND t.state = 'queued' AND m.role = 'user'
 AND (c.scope = 'project' OR (c.scope = 'session' AND (
  c.session_id = r.source_session_id
  OR c.session_id = json_extract(r.state_json, '$.successorSessionId')
 )))
 AND NOT EXISTS (
  SELECT 1 FROM orchestrator_replacements control
  WHERE control.project_id = r.project_id AND m.client_message_id <> '' AND (
   m.client_message_id = json_extract(control.state_json, '$.preparation.controlClientId')
   OR m.client_message_id = json_extract(control.state_json, '$.bootstrapClientId')
   OR m.client_message_id = json_extract(control.state_json, '$.resumeProbeClientId')
   OR EXISTS (SELECT 1 FROM json_each(control.state_json, '$.attempts') archived
    WHERE m.client_message_id = json_extract(archived.value, '$.preparation.controlClientId'))
  )
 )
ORDER BY t.requested_at, t.rowid, m.sequence, m.id;

-- name: RecordReplacementIntakeTransfer :exec
INSERT INTO orchestrator_replacement_intake_transfers(
 operation_id, message_id, turn_id, source_conversation_id, source_sequence,
 source_session_id, source_controller_generation, source_branch_id,
 target_conversation_id, target_sequence, payload_hash, transferred_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- Only never-dispatched ordinary intake can acquire the successor's owner.
-- name: MoveReplacementQueuedTurn :execrows
UPDATE conversation_turns
SET conversation_id = sqlc.arg(target_conversation_id),
 branch_id = sqlc.arg(target_branch_id),
 handled_by_session_id = sqlc.arg(target_session_id),
 controller_generation = sqlc.arg(target_generation),
 intake_sequence = CASE WHEN conversation_id <> sqlc.arg(target_conversation_id)
  THEN sqlc.arg(target_sequence) ELSE intake_sequence END
WHERE id = sqlc.arg(turn_id) AND conversation_id = sqlc.arg(source_conversation_id)
 AND state = 'queued' AND provider_turn_id = '' AND provider_input_text = ''
 AND promotion_started_at IS NULL AND promoted_to_turn_id IS NULL
 AND rolled_back_at IS NULL AND handled_by_review_id IS NULL AND batched_input = 0;

-- name: MoveReplacementQueuedMessage :execrows
UPDATE conversation_messages
SET conversation_id = sqlc.arg(target_conversation_id),
 branch_id = sqlc.arg(target_branch_id), sequence = sqlc.arg(target_sequence)
WHERE id = sqlc.arg(message_id) AND turn_id = sqlc.arg(turn_id)
 AND conversation_id = sqlc.arg(source_conversation_id) AND role = 'user';

-- Copy receipt metadata; the source history and its own receipts remain intact.
-- name: SelectReplacementQueuedEditReceipts :many
SELECT * FROM conversation_queued_edit_deliveries WHERE conversation_id = ?;

-- Check the current project controller and the exact canonical transferred
-- payload in one read snapshot. This never grants a mutable filesystem read.
-- name: SelectReplacementExcerptTransfer :one
SELECT r.id AS operation_id, m.turn_id, m.text, m.client_message_id, m.origin, m.delivery_content_json,
 m.sender_session_id, m.sender_project_id, m.sender_display_name, receipt.payload_hash
FROM orchestrator_replacement_intake_transfers receipt
JOIN orchestrator_replacements r ON r.id = receipt.operation_id
JOIN conversations target ON target.id = receipt.target_conversation_id
JOIN conversations source ON source.id = receipt.source_conversation_id
JOIN sessions owner ON owner.id = target.current_session_id
JOIN conversation_messages m ON m.id = receipt.message_id
WHERE receipt.turn_id = sqlc.arg(turn_id)
 AND receipt.source_conversation_id = sqlc.arg(source_conversation_id)
 AND receipt.target_conversation_id = sqlc.arg(target_conversation_id)
 AND receipt.payload_hash = sqlc.arg(payload_hash)
 AND target.scope = 'project' AND source.project_id = r.project_id
 AND target.project_id = r.project_id AND owner.project_id = r.project_id
 AND owner.id = sqlc.arg(target_session_id) AND owner.kind = 'orchestrator'
 AND owner.session_mode = 'chat' AND owner.is_terminated = 0
 AND owner.controller_generation <> '' AND owner.controller_generation = sqlc.arg(target_generation)
 AND m.conversation_id = target.id AND m.turn_id = receipt.turn_id AND m.role = 'user'
 AND json_type(r.state_json, '$.sourceRetiredAt') IS 'text'
 AND json_type(r.state_json, '$.writerActivation') IS 'object'
 AND json_type(r.state_json, '$.intakeReleasedAt') IS 'text'
LIMIT 1;

-- A queued native answer/activity is not proof that replay is safe.
-- name: CountReplacementTurnMessages :one
SELECT COUNT(*) FROM conversation_messages WHERE turn_id = ?;

-- name: CountReplacementTurnActivities :one
SELECT COUNT(*) FROM conversation_activities WHERE turn_id = ?;


-- name: InsertReplacementRepairSnapshot :exec
INSERT INTO orchestrator_replacement_repair_snapshots(operation_id, revision, digest, payload)
VALUES (?, ?, ?, ?);

-- name: GetReplacementRepairSnapshot :one
SELECT * FROM orchestrator_replacement_repair_snapshots
WHERE operation_id = ? AND revision = ?;

-- name: GetLatestReplacementRepairSnapshot :one
SELECT * FROM orchestrator_replacement_repair_snapshots
WHERE operation_id = ? ORDER BY revision DESC LIMIT 1;

-- Only the operator repair path can restore a broken redundant envelope. It
-- never updates sessions, leases, queue rows or publication receipts.
-- name: RestoreReplacementFromRepairSnapshot :execrows
UPDATE orchestrator_replacements
SET project_id = sqlc.arg(project_id), source_session_id = sqlc.arg(source_session_id),
 operation_key = sqlc.arg(operation_key), request_hash = sqlc.arg(request_hash),
 phase = 'recovery_required', is_active = 1, revision = sqlc.arg(new_revision),
 intake_revision = sqlc.arg(intake_revision), state_json = sqlc.arg(state_json),
 created_at = sqlc.arg(created_at), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND revision = sqlc.arg(expected_revision);


-- name: RecordReplacementHumanIntake :exec
INSERT INTO replacement_human_intake_receipts(message_id, interaction_at)
SELECT m.id, sqlc.arg(interaction_at) FROM conversation_messages m
WHERE m.id = sqlc.arg(message_id) AND m.role = 'user'
 AND EXISTS (SELECT 1 FROM orchestrator_replacement_held_turns h WHERE h.turn_id = m.turn_id)
ON CONFLICT(message_id) DO NOTHING;

-- Author/time receipts preserve AuthoredByUser automation. Legacy human-origin
-- rows retain their canonical acceptance time; never infer human intent for
-- legacy automation without a receipt.
-- name: SelectReplacementHumanProjection :one
WITH human_inputs AS (
 SELECT m.id, m.turn_id, m.text, r.interaction_at
 FROM conversation_messages m JOIN replacement_human_intake_receipts r ON r.message_id = m.id
 WHERE m.role = 'user'
 UNION ALL
 SELECT m.id, m.turn_id, m.text, m.created_at AS interaction_at
 FROM conversation_messages m WHERE m.role = 'user' AND m.origin = 'human'
 AND NOT EXISTS (SELECT 1 FROM replacement_human_intake_receipts r WHERE r.message_id = m.id)
)
SELECT h.id, h.turn_id, h.text, h.interaction_at
FROM human_inputs h JOIN conversation_turns t ON t.id = h.turn_id
WHERE t.handled_by_session_id = sqlc.arg(session_id) AND t.state = 'queued'
 AND t.rolled_back_at IS NULL AND t.promoted_to_turn_id IS NULL
 AND NOT EXISTS (SELECT 1 FROM orchestrator_replacement_held_turns held WHERE held.turn_id = t.id)
ORDER BY h.interaction_at DESC, t.requested_at DESC, t.rowid DESC, h.id DESC LIMIT 1;
