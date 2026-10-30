-- name: GetReplacementWorkerNotice :one
SELECT * FROM orchestrator_replacement_notices WHERE operation_id = ? AND session_id = ?;

-- name: ListReplacementWorkerNotices :many
SELECT * FROM orchestrator_replacement_notices WHERE operation_id = ? ORDER BY contact_id;

-- name: InsertReplacementWorkerNotice :exec
INSERT INTO orchestrator_replacement_notices(operation_id,session_id,contact_id,state,state_json,updated_at)
VALUES (?,?,?,?,?,?);

-- name: UpdateReplacementWorkerNotice :execrows
UPDATE orchestrator_replacement_notices SET state=?, state_json=?, updated_at=?, revision=revision+1
WHERE operation_id=? AND session_id=? AND revision=?;

-- name: AppendReplacementWorkerNoticeHistory :exec
INSERT INTO orchestrator_replacement_notice_history(operation_id,session_id,sequence,state_json,recorded_at)
SELECT sqlc.arg(operation_id), sqlc.arg(session_id), COALESCE(MAX(sequence),0)+1, sqlc.arg(state_json), sqlc.arg(recorded_at)
FROM orchestrator_replacement_notice_history
WHERE operation_id=sqlc.arg(operation_id) AND session_id=sqlc.arg(session_id);

-- name: ListReplacementWorkerNoticeHistory :many
SELECT * FROM orchestrator_replacement_notice_history WHERE operation_id=? AND session_id=? ORDER BY sequence;

-- name: GetLatestReplacementWorkerNoticeHistory :one
SELECT * FROM orchestrator_replacement_notice_history WHERE operation_id=? AND session_id=? ORDER BY sequence DESC LIMIT 1;

-- Delivery retries have their own bounded budget. Positive final receipts do
-- not compete with retry churn or the operation's lifecycle checkpoints.
-- name: CountReplacementWorkerNoticeRetries :one
SELECT COUNT(*) FROM orchestrator_replacement_notice_history
WHERE operation_id=? AND json_extract(state_json,'$.state') NOT IN ('accepted','skipped');

-- A worker may have changed branch/harness. More than one match is an error,
-- never permission to pick one or send with another client identity.
-- name: SelectReplacementWorkerNoticeMessages :many
SELECT m.id,m.conversation_id,m.turn_id,m.text,m.client_message_id
FROM conversation_messages m JOIN conversations c ON c.id=m.conversation_id
JOIN conversation_turns t ON t.id=m.turn_id AND t.conversation_id=c.id
WHERE c.scope='session' AND c.session_id=sqlc.arg(session_id) AND c.project_id=sqlc.arg(project_id)
 AND m.role='user' AND m.origin='automation' AND m.client_message_id=sqlc.arg(contact_id)
 AND m.text=sqlc.arg(expected_text) AND t.handled_by_session_id=sqlc.arg(session_id)
LIMIT 2;
