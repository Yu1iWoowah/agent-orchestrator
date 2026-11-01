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
 (t.state='held') <> EXISTS(SELECT 1 FROM conversation_input_holds h WHERE h.turn_id=t.id AND h.active=1)) AS INTEGER) AS consistent;
