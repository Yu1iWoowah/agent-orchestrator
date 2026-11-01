-- Shared storage only. PR1's handoff owner coordinator and 147's delivery
-- coordinator supply authorization/evidence before calling these actions.
-- name: InsertInputCustodyAction :exec
INSERT INTO conversation_input_actions(action_key,turn_id,kind,expected_owner,expected_revision,
 target_owner,hold_key,hold_owner,hold_version,reason,target_state,created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?);

-- name: SelectInputCustodyAction :one
SELECT * FROM conversation_input_actions WHERE action_key = ?;

-- name: SelectInputCustody :one
SELECT * FROM conversation_input_custody WHERE turn_id = ?;

-- name: SelectInputHolds :many
SELECT * FROM conversation_input_holds WHERE turn_id = ? ORDER BY hold_key;

-- name: ListConversationInputCustody :many
SELECT c.* FROM conversation_input_custody c
JOIN conversation_turns t ON t.id = c.turn_id
WHERE t.conversation_id = ?
ORDER BY t.requested_at,t.rowid;

-- name: InputCustodyStateConsistent :one
SELECT CAST(NOT EXISTS(SELECT 1 FROM conversation_turns t WHERE
 (t.state='held') <> EXISTS(SELECT 1 FROM conversation_input_holds h WHERE h.turn_id=t.id AND h.active=1))
 AND NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN conversation_turns t ON t.id=c.turn_id
  WHERE t.conversation_id<>c.original_conversation_id)
 AND NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN json_each(c.message_ids_json) expected
  LEFT JOIN conversation_messages payload ON payload.id=expected.value AND payload.turn_id=c.turn_id AND payload.conversation_id=c.original_conversation_id
  WHERE payload.id IS NULL)
 AND NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN json_each(c.activity_ids_json) expected
  LEFT JOIN conversation_activities payload ON payload.id=expected.value AND payload.turn_id=c.turn_id AND payload.conversation_id=c.original_conversation_id
  WHERE payload.id IS NULL)
 AS INTEGER) AS consistent;

-- name: InputCustodyTurnConsistent :one
SELECT CAST(
 NOT EXISTS(SELECT 1 FROM conversation_turns t WHERE t.id=sqlc.arg(turn_id) AND
  (t.state='held') <> EXISTS(SELECT 1 FROM conversation_input_holds h WHERE h.turn_id=t.id AND h.active=1))
 AND NOT EXISTS(SELECT 1 FROM conversation_input_custody c
  LEFT JOIN conversation_turns t ON t.id=c.turn_id
  LEFT JOIN conversation_input_actions a ON a.action_key=c.last_action_key
  WHERE c.turn_id=sqlc.arg(turn_id) AND (t.id IS NULL OR a.action_key IS NULL OR t.conversation_id<>c.original_conversation_id))
 AND NOT EXISTS(SELECT 1 FROM conversation_input_holds h
  LEFT JOIN conversation_input_custody c ON c.turn_id=h.turn_id
  LEFT JOIN conversation_input_actions a ON a.action_key=h.last_action_key
  WHERE h.turn_id=sqlc.arg(turn_id) AND (c.turn_id IS NULL OR a.action_key IS NULL))
 AND NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN json_each(c.message_ids_json) expected
  LEFT JOIN conversation_messages payload ON payload.id=expected.value AND payload.turn_id=c.turn_id AND payload.conversation_id=c.original_conversation_id
  WHERE c.turn_id=sqlc.arg(turn_id) AND payload.id IS NULL)
 AND NOT EXISTS(SELECT 1 FROM conversation_input_custody c JOIN json_each(c.activity_ids_json) expected
  LEFT JOIN conversation_activities payload ON payload.id=expected.value AND payload.turn_id=c.turn_id AND payload.conversation_id=c.original_conversation_id
  WHERE c.turn_id=sqlc.arg(turn_id) AND payload.id IS NULL)
 AS INTEGER) AS consistent;
