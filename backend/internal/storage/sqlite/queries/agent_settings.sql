-- name: GetSessionAgentSettings :one
SELECT id,model,effort,agent_fast_mode,agent_settings_revision,harness,session_mode,
 controller_generation,runtime_launch_id,is_terminated,
 CAST(COALESCE((SELECT account_id FROM session_agent_accounts a WHERE a.session_id=s.id
 AND a.provider=CASE s.harness WHEN 'codex' THEN 'codex' WHEN 'claude-code' THEN 'claude' ELSE '' END
 ORDER BY bound_at DESC LIMIT 1), '') AS TEXT) AS account_id
FROM sessions s WHERE id=?;

-- name: SaveSpawnAgentSettings :execrows
-- A spawn decision is write-once and belongs to the bound account. Later
-- project changes or a concurrent account switch cannot reinterpret it.
UPDATE sessions SET model=sqlc.arg(model),effort=sqlc.arg(effort),
 agent_fast_mode=sqlc.arg(fast_mode),spawn_agent_settings_json=sqlc.arg(resolution),
 launch_options=CASE WHEN session_mode='tui' THEN json_set(
 CASE WHEN json_valid(launch_options) THEN launch_options ELSE '{}' END,
 '$.fastMode',json(CASE WHEN sqlc.arg(fast_mode)='on' THEN 'true' ELSE 'false' END),
 '$.fastModeOff',json(CASE WHEN sqlc.arg(fast_mode)='off' THEN 'true' ELSE 'false' END)) ELSE launch_options END
WHERE id=sqlc.arg(id) AND spawn_agent_settings_json='' AND is_terminated=0
 AND runtime_handle_id='' AND provider_conversation_id=''
 AND COALESCE((SELECT account_id FROM session_agent_accounts a WHERE a.session_id=sessions.id
 AND a.provider=CASE sessions.harness WHEN 'codex' THEN 'codex' WHEN 'claude-code' THEN 'claude' ELSE '' END
 ORDER BY bound_at DESC LIMIT 1),'')=sqlc.arg(account_id);

-- name: GetAgentSettingsOperation :one
SELECT * FROM session_agent_settings_operations WHERE session_id=? AND id=?;

-- name: GetAgentSettingsOperationByKey :one
SELECT * FROM session_agent_settings_operations WHERE session_id=? AND idempotency_key=?;

-- name: InsertAgentSettingsOperation :exec
INSERT INTO session_agent_settings_operations(id,session_id,idempotency_key,request_json,phase,created_at,updated_at)
VALUES(?,?,?,?,'pending',?,?);

-- name: SetAgentSettingsOperationPhase :execrows
UPDATE session_agent_settings_operations SET phase=?,updated_at=? WHERE session_id=? AND id=? AND phase='pending';

-- name: InsertAgentSettingsAudit :exec
INSERT INTO session_agent_settings_audit(operation_id,phase,recorded_at) VALUES(?,?,?);

-- name: CommitSessionAgentSettings :execrows
UPDATE sessions SET model=sqlc.arg(model),effort=sqlc.arg(effort),agent_fast_mode=sqlc.arg(fast_mode),
 agent_settings_revision=agent_settings_revision+1
WHERE id=sqlc.arg(id) AND agent_settings_revision=sqlc.arg(expected_revision) AND is_terminated=0
 AND model=sqlc.arg(source_model) AND effort=sqlc.arg(source_effort) AND agent_fast_mode=sqlc.arg(source_fast_mode)
 AND CAST(CASE session_mode WHEN 'chat' THEN controller_generation ELSE runtime_launch_id END AS TEXT)=CAST(sqlc.arg(expected_generation) AS TEXT);

-- name: ProjectConversationAgentSettings :exec
UPDATE conversations SET model=NULLIF(sqlc.arg(model),''),reasoning_effort=NULLIF(sqlc.arg(effort),''),
 agent_fast_mode=sqlc.arg(fast_mode),updated_at=sqlc.arg(updated_at)
WHERE current_session_id=sqlc.arg(session_id);
