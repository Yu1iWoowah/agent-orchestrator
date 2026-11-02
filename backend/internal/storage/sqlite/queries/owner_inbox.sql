-- name: SelectInboxOwner :one
SELECT * FROM project_orchestrator_owners WHERE project_id=?;

-- name: SelectInboxAction :one
SELECT * FROM owner_inbox_actions WHERE project_id=? AND action_key=?;

-- name: SelectInboxSource :one
SELECT * FROM owner_inbox_sources WHERE project_id=? AND sender_id=? AND caller_key=?;

-- name: SelectInboxMembers :many
SELECT * FROM owner_inbox_members WHERE project_id=? AND batch_key=? ORDER BY position;
