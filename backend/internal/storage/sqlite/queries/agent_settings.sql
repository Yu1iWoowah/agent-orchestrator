-- name: GetSessionAgentSettings :one
-- Match SelectConversationBySession's primary project-conversation ownership.
SELECT s.id,s.model,s.effort,s.agent_fast_mode,s.agent_settings_revision,s.harness,s.session_mode,
 s.controller_generation,s.runtime_launch_id,s.is_terminated,
 CAST(CASE WHEN s.session_mode='tui' THEN s.agent_session_id ELSE s.provider_conversation_id END AS TEXT) AS provider_conversation_id,
 CAST(COALESCE((SELECT active_branch_id FROM conversations c WHERE c.current_session_id=s.id ORDER BY c.scope='project' DESC,c.created_at DESC,c.id DESC LIMIT 1),'') AS TEXT) AS branch_id,
 CAST(COALESCE((SELECT account_id FROM session_agent_accounts a WHERE a.session_id=s.id
 AND a.provider=CASE s.harness WHEN 'codex' THEN 'codex' WHEN 'claude-code' THEN 'claude' ELSE '' END
 ORDER BY bound_at DESC LIMIT 1), '') AS TEXT) AS account_id
FROM sessions s WHERE s.id=?;

-- name: InsertAgentSettingsSigningKey :exec
INSERT INTO agent_settings_authority_keys(singleton,key_bytes) VALUES(1,?) ON CONFLICT(singleton) DO NOTHING;

-- name: GetAgentSettingsSigningKey :one
SELECT key_bytes FROM agent_settings_authority_keys WHERE singleton=1;

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
INSERT INTO session_agent_settings_operations(id,session_id,idempotency_key,request_json,phase,created_at,updated_at,apply_mode,request_id,provider_conversation_id,branch_id)
VALUES(?,?,?,?,'pending',?,?,?,?,?,?);

-- name: SetAgentSettingsOperationPhase :execrows
UPDATE session_agent_settings_operations SET phase=sqlc.arg(phase),updated_at=sqlc.arg(at),
 error_category=sqlc.arg(error_category),error_code=sqlc.arg(error_code),operation_revision=operation_revision+1,
 finished_at=sqlc.narg(finished_at)
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id) AND operation_revision=sqlc.arg(expected_version)
 AND phase IN ('pending','recovery_required');

-- name: AdvanceAgentSettingsCheckpoint :execrows
UPDATE session_agent_settings_operations SET checkpoint=sqlc.arg(checkpoint),operation_revision=operation_revision+1,
 started_at=COALESCE(started_at,sqlc.arg(at)),updated_at=sqlc.arg(at)
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id) AND operation_revision=sqlc.arg(expected_version)
 AND phase IN ('pending','recovery_required');

-- name: GetPendingAgentSettingsOperation :one
SELECT * FROM session_agent_settings_operations WHERE session_id=? AND phase IN ('pending','recovery_required');

-- name: GetLatestAppliedAgentSettingsOperation :one
SELECT * FROM session_agent_settings_operations WHERE session_id=? AND phase='applied'
ORDER BY CAST(json_extract(request_json,'$.baseRevision') AS INTEGER) DESC LIMIT 1;

-- name: AdvanceAgentSettingsRecovery :execrows
UPDATE session_agent_settings_operations SET phase='recovery_required',checkpoint=sqlc.arg(checkpoint),
 recovery_json=sqlc.arg(recovery_json),error_category=sqlc.arg(error_category),
 operation_revision=operation_revision+1,updated_at=sqlc.arg(at),finished_at=NULL
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id) AND operation_revision=sqlc.arg(expected_version)
 AND phase IN ('pending','recovery_required');

-- name: InsertAgentSettingsAudit :exec
INSERT INTO session_agent_settings_audit(operation_id,phase,recorded_at,error_category,request_id,error_code) VALUES(?,?,?,?,?,?);

-- name: PinAgentSettingsExecution :execrows
UPDATE session_agent_settings_operations SET execution_json=sqlc.arg(execution_json),
 operation_revision=operation_revision+1,updated_at=sqlc.arg(at)
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id) AND operation_revision=sqlc.arg(expected_version)
 AND phase='pending' AND checkpoint='queued' AND started_at IS NULL;

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

-- name: RecordTUISettingsRuntime :execrows
-- Private process ownership evidence, never a caller-controlled settings patch.
UPDATE session_agent_settings_operations
SET request_json=json_set(request_json,sqlc.arg(slot),sqlc.arg(handle),'$.tui.unconfirmedRuntime',json('false'))
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id)
 AND phase IN ('pending','recovery_required') AND started_at IS NOT NULL
 AND json_extract(request_json,'$.tui.targetGeneration')=sqlc.arg(target_generation)
 AND ((sqlc.arg(slot)='$.tui.rollbackRuntimeHandle' AND phase='recovery_required' AND checkpoint='rolling_back') OR json_extract(request_json,sqlc.arg(slot)) IS NULL OR json_extract(request_json,sqlc.arg(slot))=sqlc.arg(handle));

-- name: SetTUISettingsRuntimeUnknown :execrows
UPDATE session_agent_settings_operations
SET request_json=json_set(request_json,'$.tui.unconfirmedRuntime',json(sqlc.arg(value)))
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id) AND phase IN ('pending','recovery_required')
 AND json_extract(request_json,'$.tui.targetGeneration') IS NOT NULL AND started_at IS NOT NULL;

-- name: SetTUISettingsTerminationProof :execrows
UPDATE session_agent_settings_operations
SET request_json=json_set(request_json,'$.tui.terminationVerified',json(sqlc.arg(verified)))
WHERE session_id=sqlc.arg(session_id) AND id=sqlc.arg(id)
 AND operation_revision=sqlc.arg(expected_version) AND phase IN ('pending','recovery_required')
 AND json_extract(request_json,'$.tui.targetGeneration') IS NOT NULL AND started_at IS NOT NULL;

-- name: HasAbandonedTUISettingsWriter :one
SELECT EXISTS(SELECT 1 FROM session_agent_settings_operations
 WHERE session_id=? AND phase='cancelled' AND error_code='AGENT_SETTINGS_SESSION_TERMINATED'
 AND COALESCE(json_extract(request_json,'$.tui.terminationVerified'),0)=0);
