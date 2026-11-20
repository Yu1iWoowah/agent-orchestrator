-- name: BeginReviewerStop :execrows
INSERT INTO reviewer_stop_receipts (
    review_id, launch_id, session_id, project_id, harness, handle_id, interface_mode, controller_generation,
    data_root, boot_id, pid_namespace, chat_scope, state, reason
)
SELECT r.id, r.reviewer_launch_id, r.session_id, r.project_id, r.harness, r.reviewer_handle_id, r.interface_mode, r.controller_generation,
    sqlc.arg(data_root), sqlc.arg(boot_id), sqlc.arg(pid_namespace), sqlc.arg(chat_scope), sqlc.arg(state), sqlc.arg(reason)
FROM review AS r
WHERE r.id=sqlc.arg(review_id) AND r.reviewer_launch_id=sqlc.arg(launch_id)
  AND r.reviewer_handle_id=sqlc.arg(handle_id) AND r.controller_generation=sqlc.arg(controller_generation);

-- name: SnapshotReviewerStopAccounts :exec
INSERT INTO reviewer_stop_accounts (review_id, launch_id, provider, account_id)
SELECT a.review_id, sqlc.arg(launch_id), a.provider, a.account_id
FROM reviewer_agent_accounts AS a WHERE a.review_id=sqlc.arg(review_id);

-- name: AddReviewerStopProcess :exec
INSERT INTO reviewer_stop_processes (review_id, launch_id, pid, start_identity, started_at)
VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING;

-- name: ListReviewerStopReceipts :many
SELECT * FROM reviewer_stop_receipts ORDER BY session_id, review_id, launch_id;

-- name: ListReviewerStopProcesses :many
SELECT * FROM reviewer_stop_processes WHERE review_id=? AND launch_id=? ORDER BY pid, start_identity;

-- name: UpdateReviewerStopReceipt :execrows
UPDATE reviewer_stop_receipts SET state=sqlc.arg(next_state), reason=sqlc.arg(reason),
    chat_scope=COALESCE(sqlc.narg(chat_scope), chat_scope)
WHERE review_id=sqlc.arg(review_id) AND launch_id=sqlc.arg(launch_id) AND state=sqlc.arg(previous_state);

-- name: ClearSettledReviewerRuntime :exec
UPDATE review SET reviewer_handle_id='', reviewer_activity_state='exited', controller_generation='', updated_at=CURRENT_TIMESTAMP
WHERE review.id=sqlc.arg(review_id) AND review.reviewer_launch_id=sqlc.arg(launch_id)
  AND review.reviewer_handle_id=sqlc.arg(handle_id) AND review.controller_generation=sqlc.arg(controller_generation);

-- name: CancelSettledReviewerRuns :exec
UPDATE review_run SET status='cancelled', ended_at=sqlc.arg(ended_at)
WHERE review_id=sqlc.arg(review_id) AND launch_id=sqlc.arg(launch_id) AND status='cancelling';

-- name: DeleteReviewerStopReceipt :execrows
DELETE FROM reviewer_stop_receipts WHERE review_id=? AND launch_id=? AND state=?;

-- name: HasUnsealedReviewerStop :one
SELECT EXISTS(SELECT 1 FROM reviewer_stop_receipts WHERE session_id=? AND state<>'sealed');

-- name: HasReviewerStop :one
SELECT EXISTS(SELECT 1 FROM reviewer_stop_receipts WHERE review_id=? AND launch_id=?);
