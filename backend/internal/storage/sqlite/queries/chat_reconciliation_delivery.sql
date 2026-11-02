-- name: InsertSettingsReconciliation :exec
INSERT INTO agent_settings_reconciliations(id,session_id,owner_generation,settings_revision,phase,version,executor_id,claim_until,record_json,created_at,updated_at,sequence)
VALUES(?,?,?,?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(sequence),0)+1 FROM agent_settings_reconciliations));

-- name: GetSettingsReconciliation :one
SELECT * FROM agent_settings_reconciliations WHERE session_id=? AND id=?;

-- name: GetActiveSettingsReconciliation :one
SELECT * FROM agent_settings_reconciliations WHERE session_id=? AND owner_generation=? AND settings_revision=? AND phase NOT IN ('ready','cancelled');

-- name: ListSettingsReconciliations :many
SELECT * FROM agent_settings_reconciliations WHERE session_id=? ORDER BY sequence DESC LIMIT 50;

-- name: UpdateSettingsReconciliation :execrows
UPDATE agent_settings_reconciliations SET phase=sqlc.arg(phase),version=version+1,
 executor_id=sqlc.arg(executor_id),claim_until=sqlc.arg(claim_until),record_json=sqlc.arg(record_json),updated_at=sqlc.arg(updated_at)
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id) AND version=sqlc.arg(expected_version)
 AND executor_id=sqlc.arg(expected_executor);

-- name: InsertChatDeliveryAttempt :exec
INSERT INTO chat_delivery_attempts(id,session_id,conversation_id,owner_generation,disposition,evidence_level,version,record_json,created_at,updated_at,sequence)
VALUES(?,?,?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(sequence),0)+1 FROM chat_delivery_attempts));

-- name: InsertChatDeliveryMember :exec
INSERT INTO chat_delivery_attempt_members(attempt_id,turn_id,ordinal) VALUES(?,?,?);

-- name: HasUnresolvedChatDelivery :one
SELECT EXISTS(SELECT 1 FROM chat_delivery_attempt_members m JOIN chat_delivery_attempts a ON a.id=m.attempt_id
 WHERE m.turn_id=? AND a.disposition<>'known_not_sent');

-- name: GetChatDeliveryAttempt :one
SELECT * FROM chat_delivery_attempts WHERE session_id=? AND id=?;

-- name: ListPendingChatDeliveryAttempts :many
SELECT * FROM chat_delivery_attempts WHERE session_id=?
 AND disposition<>'known_not_sent' AND COALESCE(json_extract(record_json,'$.executionSettled'),0)=0
 ORDER BY sequence;

-- name: GetChatDeliveryAttemptForTurn :one
SELECT a.* FROM chat_delivery_attempts a JOIN chat_delivery_attempt_members m ON m.attempt_id=a.id
 WHERE a.session_id=? AND m.turn_id=? ORDER BY a.sequence DESC LIMIT 1;

-- name: UpdateChatDeliveryAttempt :execrows
UPDATE chat_delivery_attempts SET disposition=sqlc.arg(disposition),evidence_level=sqlc.arg(evidence_level),
 version=version+1,record_json=sqlc.arg(record_json),updated_at=sqlc.arg(updated_at)
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id) AND version=sqlc.arg(expected_version);

-- name: InsertChatDeliveryReceipt :exec
INSERT INTO chat_delivery_receipts(attempt_id,version,receipt_json,recorded_at) VALUES(?,?,?,?);

-- Positive not-forwarded proof releases only the exact original reservation.
-- Held, cancelled and unrelated inputs are never released by this operation.
-- name: RequeueUnforwardedDeliveryMember :execrows
UPDATE conversation_turns SET state='queued', provider_turn_id='', started_at=NULL,
 completed_at=NULL, error_message='', promotion_started_at=NULL,
 promoted_to_turn_id=NULL, batched_input=0, provider_input_text='', agent_settings_snapshot_json=''
WHERE id=sqlc.arg(turn_id) AND conversation_id=sqlc.arg(conversation_id)
 AND branch_id=sqlc.arg(branch_id) AND handled_by_session_id=sqlc.arg(session_id)
 AND (provider_turn_id='' OR provider_turn_id=sqlc.arg(prepared_turn_id))
 AND (state IN ('queued','running') OR (state='completed' AND batched_input=1));

-- name: InsertChatWakeAttempt :exec
INSERT INTO chat_wake_attempts(id,session_id,version,record_json) VALUES(?,?,?,?);
-- name: GetChatWakeAttempt :one
SELECT * FROM chat_wake_attempts WHERE session_id=? AND id=?;
-- name: UpdateChatWakeAttempt :execrows
UPDATE chat_wake_attempts SET version=version+1,record_json=sqlc.arg(record_json)
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id) AND version=sqlc.arg(expected_version);
-- name: ListChatWakeAttempts :many
SELECT * FROM chat_wake_attempts WHERE session_id=? ORDER BY sequence DESC LIMIT 50;
