-- name: GetReviewerAgentSettings :one
SELECT r.*, p.settings_json, p.revision AS settings_revision,
       COALESCE(a.account_id, '') AS account_id,
       CAST(EXISTS(SELECT 1 FROM review_run AS pass WHERE pass.review_id=r.id AND pass.status='running')
         OR EXISTS(SELECT 1 FROM conversation_turns AS turn JOIN conversations AS c ON c.id=turn.conversation_id
                   WHERE c.current_review_id=r.id AND turn.state IN ('queued','running')) AS INTEGER) AS has_running_pass
FROM review AS r
LEFT JOIN reviewer_agent_settings AS p ON p.review_id=r.id
LEFT JOIN reviewer_agent_accounts AS a ON a.review_id=r.id AND a.provider=CASE r.harness WHEN 'codex' THEN 'codex' WHEN 'claude-code' THEN 'claude' ELSE '' END
WHERE r.id=?;

-- name: InitializeReviewerAgentSettings :exec
INSERT INTO reviewer_agent_settings (review_id, revision, settings_json, created_at, updated_at)
SELECT r.id, 0, sqlc.arg(settings_json), sqlc.arg(recorded_at), sqlc.arg(recorded_at)
FROM review AS r JOIN sessions AS s ON s.id=r.session_id
WHERE r.id=sqlc.arg(review_id) AND s.is_terminated=0 AND r.project_id=s.project_id
ON CONFLICT(review_id) DO NOTHING;

-- name: CommitReviewerAgentSettings :execrows
UPDATE reviewer_agent_settings
SET revision=reviewer_agent_settings.revision+1, settings_json=sqlc.arg(target_json), updated_at=sqlc.arg(recorded_at)
WHERE reviewer_agent_settings.review_id=sqlc.arg(review_id) AND reviewer_agent_settings.revision=sqlc.arg(base_revision) AND reviewer_agent_settings.settings_json=sqlc.arg(source_json)
  AND EXISTS(SELECT 1 FROM review AS r JOIN sessions AS s ON s.id=r.session_id
      JOIN reviewer_agent_accounts AS a ON a.review_id=r.id AND a.provider=CASE r.harness WHEN 'codex' THEN 'codex' WHEN 'claude-code' THEN 'claude' ELSE '' END
      WHERE r.id=reviewer_agent_settings.review_id AND r.is_archived=0 AND s.is_terminated=0 AND r.project_id=s.project_id
        AND a.account_id=sqlc.arg(account_id)
        AND CASE r.interface_mode WHEN 'chat' THEN r.controller_generation ELSE r.reviewer_launch_id END=sqlc.arg(generation)
        AND CASE r.interface_mode WHEN 'chat' THEN r.provider_conversation_id ELSE r.agent_session_id END=sqlc.arg(native_id))
  AND NOT EXISTS(SELECT 1 FROM review_run AS pass WHERE pass.review_id=reviewer_agent_settings.review_id AND pass.status='running')
  AND NOT EXISTS(SELECT 1 FROM conversation_turns AS turn JOIN conversations AS c ON c.id=turn.conversation_id
      WHERE c.current_review_id=reviewer_agent_settings.review_id AND turn.state IN ('queued','running'));

-- name: InsertReviewerAgentSettingsOperation :exec
INSERT INTO reviewer_agent_settings_operations (id, review_id, revision, idempotency_key, request_fingerprint, operation_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetReviewerAgentSettingsOperation :one
SELECT operation_json FROM reviewer_agent_settings_operations WHERE review_id=? AND idempotency_key=?;

-- name: ListReviewerAgentSettingsOperations :many
SELECT operation_json FROM reviewer_agent_settings_operations WHERE review_id=? ORDER BY revision DESC LIMIT ?;
