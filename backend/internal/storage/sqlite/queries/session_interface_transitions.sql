-- name: InsertSessionInterfaceTransition :one
INSERT INTO session_interface_transitions (
    id, session_id, source_mode, target_mode, policy, history_policy, phase,
    native_conversation_id, error_code, error_detail,
    created_at, updated_at, completed_at, notice_acknowledged_at,
    source_account_id, target_account_id, target_runtime_launch_id, target_runtime_handle_id, target_runtime_state
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, NULL, NULL, sqlc.arg(source_account_id), sqlc.arg(target_account_id), '', '', CASE WHEN sqlc.arg(target_account_id)<>'' THEN 'not_started' ELSE '' END)
RETURNING id, session_id, source_mode, target_mode, policy, phase,
          native_conversation_id, error_code, error_detail,
          created_at, updated_at, completed_at, notice_acknowledged_at, history_policy,
       source_account_id, target_account_id, target_runtime_launch_id, target_runtime_handle_id, target_runtime_state;

-- name: GetSessionInterfaceTransition :one
SELECT id, session_id, source_mode, target_mode, policy, phase,
       native_conversation_id, error_code, error_detail,
       created_at, updated_at, completed_at, notice_acknowledged_at, history_policy,
       source_account_id, target_account_id, target_runtime_launch_id, target_runtime_handle_id, target_runtime_state
FROM session_interface_transitions
WHERE id = ?;

-- name: GetActiveSessionInterfaceTransition :one
SELECT id, session_id, source_mode, target_mode, policy, phase,
       native_conversation_id, error_code, error_detail,
       created_at, updated_at, completed_at, notice_acknowledged_at, history_policy,
       source_account_id, target_account_id, target_runtime_launch_id, target_runtime_handle_id, target_runtime_state
FROM session_interface_transitions
WHERE session_id = ?
  AND phase NOT IN ('completed', 'failed', 'cancelled', 'recovery_required')
ORDER BY created_at DESC
LIMIT 1;

-- name: GetLatestSessionInterfaceTransition :one
SELECT id, session_id, source_mode, target_mode, policy, phase,
       native_conversation_id, error_code, error_detail,
       created_at, updated_at, completed_at, notice_acknowledged_at, history_policy,
       source_account_id, target_account_id, target_runtime_launch_id, target_runtime_handle_id, target_runtime_state
FROM session_interface_transitions
WHERE session_id = ?
-- Tests and injected clocks can assign multiple sagas the same timestamp.
-- This rowid table is append-only for a surviving session, so descending rowid
-- is the unique insertion-order tie-break that identifies the newer saga.
ORDER BY created_at DESC, rowid DESC
LIMIT 1;

-- name: ListActiveSessionInterfaceTransitions :many
SELECT id, session_id, source_mode, target_mode, policy, phase,
       native_conversation_id, error_code, error_detail,
       created_at, updated_at, completed_at, notice_acknowledged_at, history_policy,
       source_account_id, target_account_id, target_runtime_launch_id, target_runtime_handle_id, target_runtime_state
FROM session_interface_transitions
WHERE phase NOT IN ('completed', 'failed', 'cancelled', 'recovery_required')
ORDER BY created_at;

-- name: ListDeliverableSessionInterfaceTransitions :many
SELECT t.id, t.session_id, t.source_mode, t.target_mode, t.policy, t.phase,
       t.native_conversation_id, t.error_code, t.error_detail,
       t.created_at, t.updated_at, t.completed_at, t.notice_acknowledged_at, t.history_policy,
       t.source_account_id, t.target_account_id, t.target_runtime_launch_id, t.target_runtime_handle_id, t.target_runtime_state
FROM session_interface_transitions AS t
WHERE t.phase IN ('completed', 'failed', 'cancelled', 'recovery_required')
  AND EXISTS (
      SELECT 1
      FROM session_interface_transition_messages AS m
      WHERE m.transition_id = t.id AND m.delivered_at IS NULL
  )
ORDER BY t.updated_at, t.id;

-- name: AdvanceSessionInterfaceTransition :execrows
UPDATE session_interface_transitions
SET phase = ?, native_conversation_id = ?, error_code = ?, error_detail = ?,
    updated_at = ?, completed_at = ?
WHERE id = ? AND phase = ?;

-- name: AcknowledgeSessionInterfaceTransitionNotice :one
UPDATE session_interface_transitions
SET notice_acknowledged_at = COALESCE(
    notice_acknowledged_at,
    sqlc.arg(notice_acknowledged_at)
)
WHERE id = sqlc.arg(id)
  AND session_id = sqlc.arg(session_id)
  AND phase IN ('failed', 'recovery_required')
RETURNING id, session_id, source_mode, target_mode, policy, phase,
          native_conversation_id, error_code, error_detail,
          created_at, updated_at, completed_at, notice_acknowledged_at, history_policy,
       source_account_id, target_account_id, target_runtime_launch_id, target_runtime_handle_id, target_runtime_state;

-- name: EnqueueSessionInterfaceTransitionMessage :exec
INSERT INTO session_interface_transition_messages (
    transition_id, client_message_id, message, created_at, sender_session_id, authored_by_user
)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListPendingSessionInterfaceTransitionMessages :many
SELECT id, transition_id, client_message_id, message, created_at, delivered_at, sender_session_id, authored_by_user
FROM session_interface_transition_messages
WHERE transition_id = ? AND delivered_at IS NULL
ORDER BY id;

-- name: MarkSessionInterfaceTransitionMessageDelivered :execrows
UPDATE session_interface_transition_messages
SET delivered_at = ?
WHERE id = ? AND delivered_at IS NULL;

-- name: RecordAccountTransitionRuntime :execrows
UPDATE session_interface_transitions
SET target_runtime_launch_id=sqlc.arg(launch_id), target_runtime_handle_id=sqlc.arg(handle_id),
    target_runtime_state=sqlc.arg(state), updated_at=sqlc.arg(updated_at)
WHERE id=sqlc.arg(id) AND target_account_id<>'' AND (phase='target_starting' OR (phase IN ('activating','recovery_required') AND sqlc.arg(state)='stopped'))
  AND target_runtime_state=sqlc.arg(expected_state) AND target_runtime_launch_id=sqlc.arg(expected_launch_id);

-- name: ListUnsettledAccountTransitions :many
SELECT t.* FROM session_interface_transitions AS t
JOIN sessions AS s ON s.id=t.session_id
WHERE t.phase='recovery_required' AND t.target_account_id<>''
  AND t.target_runtime_state NOT IN ('not_started','stopped')
  AND (s.is_terminated=1 OR NOT EXISTS (
    SELECT 1 FROM session_agent_accounts AS b
    WHERE b.session_id=t.session_id AND b.account_id=t.target_account_id
      AND b.bound_at=(SELECT MAX(c.bound_at) FROM session_agent_accounts AS c
                      WHERE c.session_id=b.session_id AND c.provider=b.provider)))
ORDER BY t.created_at,t.id;
