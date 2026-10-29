-- Subscription accounts AO launches Claude Code and Codex with.

-- name: ListAgentAccounts :many
SELECT * FROM agent_accounts ORDER BY provider, created_at, id;

-- name: CreateAgentAccount :exec
INSERT INTO agent_accounts (id, provider, home, email, plan, created_at, profile_kind,label,disabled_at,retired_at)
VALUES (?, ?, ?, ?, ?, ?, ?,?,?,?);

-- name: DeleteAgentAccount :exec
UPDATE agent_accounts SET retired_at = CURRENT_TIMESTAMP, disabled_at = CURRENT_TIMESTAMP WHERE id = ? AND retired_at IS NULL;

-- name: DeleteAgentAccountDefaultFor :exec
DELETE FROM agent_account_defaults WHERE account_id = ?;

-- name: GetAgentAccountDefault :one
SELECT account_id FROM agent_account_defaults WHERE provider = ?;

-- name: SetAgentAccountDefault :exec
INSERT INTO agent_account_defaults (provider, account_id) VALUES (?, ?)
ON CONFLICT (provider) DO UPDATE SET account_id = excluded.account_id;

-- name: BindSessionAgentAccount :exec
INSERT INTO session_agent_accounts (session_id, provider, account_id, bound_at, resolution_source)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (session_id, provider, bound_at) DO UPDATE SET account_id = excluded.account_id, resolution_source = excluded.resolution_source;

-- name: GetSessionAgentAccount :one
SELECT account_id FROM session_agent_accounts
WHERE session_id = ? AND provider = ?
ORDER BY bound_at DESC
LIMIT 1;

-- Only a known source-time binding can prove that a turn used another account.
-- name: SessionAgentAccountChangedSince :one
SELECT EXISTS (
    SELECT 1 FROM sessions s
    JOIN session_agent_accounts source ON source.session_id = s.id
      AND source.provider = CASE s.harness WHEN 'claude-code' THEN 'claude' WHEN 'codex' THEN 'codex' ELSE '' END
    WHERE s.id = sqlc.arg(session_id)
      AND source.bound_at = (
          SELECT MAX(a.bound_at) FROM session_agent_accounts a
          WHERE a.session_id = s.id AND a.provider = source.provider
            AND a.bound_at <= sqlc.arg(requested_at)
      )
      AND source.account_id <> (
          SELECT a.account_id FROM session_agent_accounts a
          WHERE a.session_id = s.id AND a.provider = source.provider
          ORDER BY a.bound_at DESC LIMIT 1
      )
);

-- Account changes invalidate only this session's active provider's pending work.
-- name: CancelSessionTurnAutoResume :exec
UPDATE conversation_turns
SET usage_limit_auto_resume = 0, usage_limit_resume_error = ''
WHERE handled_by_session_id = sqlc.arg(session_id) AND usage_limit_auto_resume = 1
  AND EXISTS (
      SELECT 1 FROM sessions s WHERE s.id = conversation_turns.handled_by_session_id
        AND CASE s.harness WHEN 'claude-code' THEN 'claude' WHEN 'codex' THEN 'codex' ELSE '' END = CAST(sqlc.arg(provider) AS TEXT)
  );

-- Each usage event counts for the account its session used when the event
-- happened; an event older than the session's first binding counts for that
-- first account. Days are UTC calendar days.
-- name: AgentAccountUsageSince :many
SELECT
    CAST(COALESCE(
        (SELECT a.account_id FROM session_agent_accounts a
         WHERE a.session_id = b.session_id
           AND a.provider = CASE b.harness WHEN 'claude-code' THEN 'claude' ELSE 'codex' END
           AND a.bound_at <= e.created_at
         ORDER BY a.bound_at DESC LIMIT 1),
        (SELECT a.account_id FROM session_agent_accounts a
         WHERE a.session_id = b.session_id
           AND a.provider = CASE b.harness WHEN 'claude-code' THEN 'claude' ELSE 'codex' END
         ORDER BY a.bound_at ASC LIMIT 1),
        'device-' || CASE b.harness WHEN 'claude-code' THEN 'claude' ELSE 'codex' END
    ) AS TEXT) AS account_id,
    CAST(substr(e.created_at, 1, 10) AS TEXT) AS day,
    e.model_id,
    CAST(COALESCE(SUM(e.input_tokens), 0) AS INTEGER) AS input_tokens,
    CAST(COALESCE(SUM(e.cached_input_tokens), 0) AS INTEGER) AS cached_input_tokens,
    CAST(COALESCE(SUM(e.output_tokens), 0) AS INTEGER) AS output_tokens,
    CAST(COALESCE(SUM(e.estimated_cost_nanos), 0) AS INTEGER) AS estimated_cost_nanos
FROM model_usage_events e
JOIN usage_bindings b ON b.id = e.binding_id
WHERE e.created_at >= sqlc.arg(since)
  AND b.harness IN ('claude-code', 'codex')
GROUP BY 1, 2, 3
ORDER BY 2, 1, 3;

-- name: CreateAgentAccountOperation :exec
INSERT INTO agent_account_operations(id,account_id,provider,action,phase,outcome,reason_code,terminal_id,result_account_id,was_disabled,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?);

-- name: UpdateAgentAccountOperation :exec
UPDATE agent_account_operations SET phase=?,outcome=?,reason_code=?,result_account_id=?,updated_at=? WHERE id=?;

-- name: GetAgentAccountOperation :one
SELECT * FROM agent_account_operations WHERE id=?;

-- name: ListAgentAccountOperations :many
SELECT * FROM agent_account_operations ORDER BY created_at,id;

-- name: DisableAgentAccount :exec
UPDATE agent_accounts SET disabled_at=?,lifecycle_operation_id=? WHERE id=?;

-- name: EnableAgentAccount :exec
UPDATE agent_accounts SET disabled_at=NULL WHERE id=? AND retired_at IS NULL;

-- name: UpsertAgentAccountLogin :exec
INSERT INTO agent_accounts(id,provider,home,email,plan,created_at,profile_kind,disabled_at,lifecycle_operation_id)
VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET email=excluded.email,plan=excluded.plan,disabled_at=excluded.disabled_at,lifecycle_operation_id=excluded.lifecycle_operation_id
WHERE agent_accounts.retired_at IS NULL;

-- name: GetSessionAgentAccountBinding :one
SELECT account_id,provider,resolution_source,bound_at FROM session_agent_accounts
WHERE session_id=? AND provider=? ORDER BY bound_at DESC LIMIT 1;

-- name: BindReviewerAgentAccount :exec
INSERT INTO reviewer_agent_accounts(review_id,provider,account_id,resolution_source,bound_at)
VALUES (?,?,?,?,?) ON CONFLICT(review_id,provider) DO NOTHING;

-- name: GetReviewerAgentAccount :one
SELECT account_id,provider,resolution_source FROM reviewer_agent_accounts WHERE review_id=? AND provider=?;

-- name: GetOpenReviewerAccountUse :one
SELECT r.id AS review_id,r.session_id,r.harness
FROM reviewer_agent_accounts AS a JOIN review AS r ON r.id=a.review_id
WHERE a.account_id=? AND r.is_archived=0
  AND (r.reviewer_activity_state<>'exited'
       OR EXISTS(SELECT 1 FROM review_run AS pass WHERE pass.review_id=r.id AND pass.status='running'))
ORDER BY r.created_at,r.id,a.provider LIMIT 1;

-- Current bindings are owners; older receipts remain historical attribution.
-- A running pass owns its profile before native activity replaces an old exited
-- observation; the launch lease may return before the caller commits its handle.
-- Transition targets are owners before a binding is committed. No profile path
-- or provider output is part of this local-only projection.
-- A committed target is owned by its current session binding, never by a
-- historical source receipt. Uncommitted/terminated targets retain an owner
-- until recovery positively settles their runtime receipt.
-- name: ListAgentAccountUsageOwners :many
SELECT 'session' AS owner_kind, CAST(COALESCE(s.project_id,'') AS TEXT) AS project_id, s.id AS session_id, s.display_name AS session_name,
       CAST('' AS TEXT) AS review_id, CAST('' AS TEXT) AS pr_url, s.harness,
       CAST('' AS TEXT) AS transition_id,
       CAST(s.is_terminated=0 AND (s.activity_state<>'exited' OR s.provision_state='provisioning') AS INTEGER) AS blocking
FROM session_agent_accounts AS a JOIN sessions AS s ON s.id=a.session_id
WHERE a.account_id=sqlc.arg(account_id)
  AND a.bound_at=(SELECT MAX(b.bound_at) FROM session_agent_accounts AS b WHERE b.session_id=a.session_id AND b.provider=a.provider)
UNION ALL
SELECT 'reviewer', CAST(COALESCE(s.project_id,'') AS TEXT), s.id, s.display_name, r.id, r.pr_url, r.harness, '',
       CAST(r.is_archived=0 AND (r.reviewer_activity_state<>'exited'
         OR EXISTS(SELECT 1 FROM review_run AS pass WHERE pass.review_id=r.id AND pass.status='running')) AS INTEGER)
FROM reviewer_agent_accounts AS a JOIN review AS r ON r.id=a.review_id JOIN sessions AS s ON s.id=r.session_id
WHERE a.account_id=sqlc.arg(account_id)
UNION ALL
SELECT 'transition', CAST(COALESCE(s.project_id,'') AS TEXT), s.id, s.display_name, '', '', s.harness, t.id, 1
FROM session_interface_transitions AS t JOIN sessions AS s ON s.id=t.session_id
WHERE ((t.source_account_id=sqlc.arg(account_id) OR t.target_account_id=sqlc.arg(account_id))
       AND t.phase NOT IN ('completed','failed','cancelled','recovery_required'))
   OR (t.target_account_id=sqlc.arg(account_id) AND t.phase='recovery_required'
       AND t.target_runtime_state NOT IN ('not_started','stopped')
       AND NOT EXISTS (SELECT 1 FROM session_interface_transitions AS successor
                       WHERE successor.session_id=t.session_id AND successor.rowid>t.rowid AND successor.phase='completed')
       AND (s.is_terminated=1 OR NOT EXISTS (
         SELECT 1 FROM session_agent_accounts AS b
         WHERE b.session_id=t.session_id AND b.account_id=t.target_account_id
           AND b.bound_at=(SELECT MAX(c.bound_at) FROM session_agent_accounts AS c
                           WHERE c.session_id=b.session_id AND c.provider=b.provider))))
ORDER BY owner_kind, session_id, review_id, transition_id;
