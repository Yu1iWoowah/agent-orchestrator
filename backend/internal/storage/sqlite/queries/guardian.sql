-- Only the current proven native root is collected. Old account epochs are never
-- inferred from cwd, file locations, or adjacent timestamps.
-- name: ListGuardianCandidates :many
SELECT ub.id AS binding_id, ub.session_id, ub.native_root_id, s.project_id,
 a.account_id, a.bound_at, p.enabled_at,
 w.cleaned_before,
 CAST((SELECT count(*) FROM sessions other WHERE other.harness='codex' AND other.is_terminated=0
 AND ub.native_root_id=CASE WHEN other.session_mode='chat' AND other.provider_conversation_id<>''
 THEN other.provider_conversation_id ELSE other.agent_session_id END)>1 AS INTEGER) AS owner_conflict
FROM usage_bindings ub JOIN sessions s ON s.id=ub.session_id
JOIN guardian_project_policy p ON p.project_id=s.project_id AND p.enabled=1
JOIN session_agent_accounts a ON a.session_id=s.id AND a.provider='codex'
LEFT JOIN guardian_watermarks w ON w.session_id=s.id
WHERE ub.harness='codex' AND s.harness='codex' AND s.is_terminated=0
 AND a.bound_at=(SELECT MAX(b.bound_at) FROM session_agent_accounts b WHERE b.session_id=s.id AND b.provider='codex')
 AND ub.native_root_id=CASE WHEN s.session_mode='chat' AND s.provider_conversation_id<>''
 THEN s.provider_conversation_id ELSE s.agent_session_id END
ORDER BY ub.id;

-- name: EnsureGuardianEpoch :one
INSERT INTO guardian_epochs(binding_id,session_id,account_id,account_bound_at,first_seen_at)
VALUES(?,?,?,?,?) ON CONFLICT(binding_id,account_id,account_bound_at) DO UPDATE SET binding_id=excluded.binding_id RETURNING *;

-- name: GuardianEpochEnabled :one
SELECT p.enabled,p.enabled_at,w.cleaned_before
FROM guardian_epochs e JOIN sessions s ON s.id=e.session_id
JOIN guardian_project_policy p ON p.project_id=s.project_id
LEFT JOIN guardian_watermarks w ON w.session_id=s.id
WHERE e.id=?;

-- name: GetGuardianCursor :one
SELECT * FROM guardian_cursors WHERE epoch_id=? AND thread_id=?;

-- name: GetGuardianPosition :one
SELECT * FROM guardian_positions WHERE epoch_id=? AND file_identity=?;

-- name: PutGuardianPosition :exec
INSERT INTO guardian_positions(epoch_id,file_identity,generation,byte_offset,discarding,updated_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(epoch_id,file_identity) DO UPDATE SET
 generation=excluded.generation,byte_offset=excluded.byte_offset,discarding=excluded.discarding,updated_at=excluded.updated_at;

-- name: PutGuardianCursor :exec
INSERT INTO guardian_cursors(epoch_id,thread_id,generation,byte_offset,file_identity,discarding,updated_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(epoch_id,thread_id) DO UPDATE SET
 generation=excluded.generation,byte_offset=excluded.byte_offset,file_identity=excluded.file_identity,discarding=excluded.discarding,updated_at=excluded.updated_at;

-- name: PutGuardianThread :exec
INSERT INTO guardian_threads(epoch_id,thread_id,parent_id,cli_version,retention_at,first_seen_at,last_seen_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(epoch_id,thread_id) DO UPDATE SET last_seen_at=excluded.last_seen_at;

-- name: GetGuardianThread :one
SELECT * FROM guardian_threads WHERE epoch_id=? AND thread_id=?;

-- name: PutGuardianModel :execrows
INSERT INTO guardian_turn_models(epoch_id,thread_id,turn_id,model_id,provider_at)
SELECT sqlc.arg(epoch_id),sqlc.arg(thread_id),sqlc.arg(turn_id),sqlc.arg(model_id),sqlc.arg(provider_at)
WHERE (SELECT COUNT(*) FROM guardian_turn_models m WHERE m.epoch_id=sqlc.arg(epoch_id) AND m.thread_id=sqlc.arg(thread_id))<4096
 OR EXISTS(SELECT 1 FROM guardian_turn_models m WHERE m.epoch_id=sqlc.arg(epoch_id) AND m.thread_id=sqlc.arg(thread_id) AND m.turn_id=sqlc.arg(turn_id))
ON CONFLICT(epoch_id,thread_id,turn_id) DO UPDATE SET
 conflict=CASE WHEN model_id IS NOT excluded.model_id THEN 1 ELSE conflict END;

-- name: FillGuardianModels :exec
UPDATE guardian_requests SET
 model_id=(SELECT CASE WHEN m.conflict=0 THEN m.model_id ELSE NULL END FROM guardian_turn_models m
 WHERE m.epoch_id=guardian_requests.epoch_id AND m.thread_id=guardian_requests.thread_id AND m.turn_id=guardian_requests.turn_id),
 conflict=MAX(conflict,COALESCE((SELECT m.conflict FROM guardian_turn_models m
 WHERE m.epoch_id=guardian_requests.epoch_id AND m.thread_id=guardian_requests.thread_id AND m.turn_id=guardian_requests.turn_id),0))
WHERE guardian_requests.epoch_id=? AND guardian_requests.thread_id=? AND guardian_requests.turn_id=?
AND (model_id IS NOT (SELECT CASE WHEN m.conflict=0 THEN m.model_id ELSE NULL END FROM guardian_turn_models m
 WHERE m.epoch_id=guardian_requests.epoch_id AND m.thread_id=guardian_requests.thread_id AND m.turn_id=guardian_requests.turn_id)
 OR conflict<COALESCE((SELECT m.conflict FROM guardian_turn_models m
 WHERE m.epoch_id=guardian_requests.epoch_id AND m.thread_id=guardian_requests.thread_id AND m.turn_id=guardian_requests.turn_id),0));

-- name: InsertGuardianRequest :exec
INSERT INTO guardian_requests(cli_version,epoch_id,thread_id,turn_id,response_id,root_turn_id,
 model_id,input_tokens,cached_tokens,cache_write_tokens,output_tokens,reasoning_tokens,total_tokens,
 provider_at,first_seen_at,source_generation,source_offset)
VALUES(?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(epoch_id,thread_id,response_id) DO UPDATE SET conflict=CASE WHEN
 cli_version IS NOT excluded.cli_version OR turn_id IS NOT excluded.turn_id OR root_turn_id IS NOT excluded.root_turn_id OR
 input_tokens IS NOT excluded.input_tokens OR cached_tokens IS NOT excluded.cached_tokens OR
 cache_write_tokens IS NOT excluded.cache_write_tokens OR output_tokens IS NOT excluded.output_tokens OR
 reasoning_tokens IS NOT excluded.reasoning_tokens OR total_tokens IS NOT excluded.total_tokens
 THEN 1 ELSE conflict END;

-- name: ListGuardianRequests :many
SELECT * FROM guardian_requests WHERE epoch_id=? ORDER BY thread_id,response_id;

-- name: ListGuardianModels :many
SELECT * FROM guardian_turn_models WHERE epoch_id=? AND thread_id=? ORDER BY turn_id;

-- name: ListGuardianCursors :many
SELECT * FROM guardian_cursors WHERE epoch_id=? ORDER BY thread_id;

-- name: AddGuardianAnomaly :exec
INSERT INTO guardian_anomalies(session_id,code,count,first_seen_at)
SELECT sqlc.arg(session_id),sqlc.arg(code),sqlc.arg(count),sqlc.arg(first_seen_at)
WHERE EXISTS(SELECT 1 FROM sessions s JOIN guardian_project_policy p ON p.project_id=s.project_id WHERE s.id=sqlc.arg(session_id) AND p.enabled=1)
ON CONFLICT(session_id,code) DO UPDATE SET count=MIN(9223372036854775807-count,excluded.count)+count;

-- name: ListGuardianAnomalies :many
SELECT * FROM guardian_anomalies WHERE session_id=? ORDER BY code;

-- name: AdvanceGuardianWatermark :exec
INSERT INTO guardian_watermarks(session_id,cleaned_before) VALUES(?,?)
ON CONFLICT(session_id) DO UPDATE SET cleaned_before=MAX(cleaned_before,excluded.cleaned_before);

-- name: AdvanceAllGuardianWatermarks :exec
INSERT INTO guardian_watermarks(session_id,cleaned_before)
SELECT DISTINCT session_id,sqlc.arg(cutoff) FROM guardian_epochs WHERE true
ON CONFLICT(session_id) DO UPDATE SET cleaned_before=MAX(cleaned_before,excluded.cleaned_before);

-- name: DeleteExpiredGuardianRequests :exec
DELETE FROM guardian_requests WHERE provider_at<=sqlc.arg(cutoff) OR first_seen_at<=sqlc.arg(cutoff);
-- name: DeleteExpiredGuardianModels :exec
DELETE FROM guardian_turn_models WHERE provider_at<=?;
-- name: DeleteExpiredGuardianThreads :exec
DELETE FROM guardian_threads WHERE guardian_threads.retention_at<=sqlc.arg(cutoff)
AND NOT EXISTS(SELECT 1 FROM guardian_requests r WHERE r.epoch_id=guardian_threads.epoch_id AND r.thread_id=guardian_threads.thread_id)
AND NOT EXISTS(SELECT 1 FROM guardian_turn_models m WHERE m.epoch_id=guardian_threads.epoch_id AND m.thread_id=guardian_threads.thread_id);
-- name: ClearExpiredGuardianThreadMetadata :exec
UPDATE guardian_threads SET cli_version='' WHERE retention_at<=? AND cli_version<>'';
-- name: DeleteExpiredGuardianEpochs :exec
DELETE FROM guardian_epochs WHERE guardian_epochs.first_seen_at<=sqlc.arg(cutoff)
AND NOT EXISTS(SELECT 1 FROM guardian_threads t WHERE t.epoch_id=guardian_epochs.id)
AND NOT EXISTS(SELECT 1 FROM guardian_positions p WHERE p.epoch_id=guardian_epochs.id);
-- name: DeleteExpiredGuardianPositions :exec
DELETE FROM guardian_positions WHERE updated_at<=?;
-- name: DeleteExpiredGuardianAnomalies :exec
DELETE FROM guardian_anomalies WHERE first_seen_at<=?;
-- name: PurgeGuardianEpochs :exec
DELETE FROM guardian_epochs WHERE session_id=?;
-- name: PurgeGuardianAnomalies :exec
DELETE FROM guardian_anomalies WHERE session_id=?;
