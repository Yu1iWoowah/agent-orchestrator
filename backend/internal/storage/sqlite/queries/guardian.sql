-- Only the current proven native root is collected. Old account epochs are never
-- inferred from cwd, file locations, or adjacent timestamps.
-- name: HasGuardianCleanupState :one
SELECT EXISTS(SELECT 1 FROM guardian_epochs UNION ALL SELECT 1 FROM guardian_anomalies);

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

-- name: GuardianSessionEnabled :one
SELECT EXISTS(SELECT 1 FROM sessions s JOIN guardian_project_policy p ON p.project_id=s.project_id
WHERE s.id=sqlc.arg(session_id) AND p.enabled=1);

-- name: ListGuardianReviewCandidates :many
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
WHERE s.id=sqlc.arg(session_id) AND ub.harness='codex' AND s.harness='codex' AND s.is_terminated=0
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
INSERT INTO guardian_positions(epoch_id,file_identity,generation,byte_offset,discarding,source_size,source_modified_at,read_start_offset,replay_until,replay_offset,replay_discarding,replay_again,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(epoch_id,file_identity) DO UPDATE SET
 generation=excluded.generation,byte_offset=excluded.byte_offset,discarding=excluded.discarding,source_size=excluded.source_size,source_modified_at=excluded.source_modified_at,read_start_offset=excluded.read_start_offset,replay_until=excluded.replay_until,replay_offset=excluded.replay_offset,replay_discarding=excluded.replay_discarding,replay_again=excluded.replay_again,updated_at=excluded.updated_at;

-- name: PutGuardianCursor :exec
INSERT INTO guardian_cursors(epoch_id,thread_id,generation,byte_offset,file_identity,discarding,source_size,source_modified_at,read_start_offset,replay_until,replay_offset,replay_discarding,replay_again,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(epoch_id,thread_id) DO UPDATE SET
 generation=excluded.generation,byte_offset=excluded.byte_offset,file_identity=excluded.file_identity,discarding=excluded.discarding,source_size=excluded.source_size,source_modified_at=excluded.source_modified_at,read_start_offset=excluded.read_start_offset,replay_until=excluded.replay_until,replay_offset=excluded.replay_offset,replay_discarding=excluded.replay_discarding,replay_again=excluded.replay_again,updated_at=excluded.updated_at;

-- name: PutGuardianThread :exec
INSERT INTO guardian_threads(epoch_id,thread_id,parent_id,cli_version,retention_at,first_seen_at,last_seen_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(epoch_id,thread_id) DO NOTHING;

-- name: TouchGuardianThread :exec
UPDATE guardian_threads SET last_seen_at=sqlc.arg(last_seen_at)
WHERE epoch_id=sqlc.arg(epoch_id) AND thread_id=sqlc.arg(thread_id) AND last_seen_at<sqlc.arg(last_seen_at);

-- name: HasGuardianParent :one
SELECT EXISTS(SELECT 1 FROM guardian_threads WHERE epoch_id=? AND parent_id=?);

-- name: GetGuardianThread :one
SELECT * FROM guardian_threads WHERE epoch_id=? AND thread_id=?;

-- name: PutGuardianModel :execrows
INSERT INTO guardian_turn_models(epoch_id,thread_id,turn_id,model_id,provider_at)
SELECT sqlc.arg(epoch_id),sqlc.arg(thread_id),sqlc.arg(turn_id),sqlc.arg(model_id),sqlc.arg(provider_at)
WHERE (SELECT COUNT(*) FROM guardian_turn_models m WHERE m.epoch_id=sqlc.arg(epoch_id) AND m.thread_id=sqlc.arg(thread_id))<4096
 OR EXISTS(SELECT 1 FROM guardian_turn_models m WHERE m.epoch_id=sqlc.arg(epoch_id) AND m.thread_id=sqlc.arg(thread_id) AND m.turn_id=sqlc.arg(turn_id))
ON CONFLICT(epoch_id,thread_id,turn_id) DO UPDATE SET
 conflict=1 WHERE conflict=0 AND model_id IS NOT excluded.model_id;

-- name: GuardianModelExists :one
SELECT EXISTS(SELECT 1 FROM guardian_turn_models WHERE epoch_id=? AND thread_id=? AND turn_id=?);

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

-- name: InsertGuardianRequest :execrows
INSERT INTO guardian_requests(cli_version,epoch_id,thread_id,turn_id,response_id,root_turn_id,
 model_id,input_tokens,cached_tokens,cache_write_tokens,output_tokens,reasoning_tokens,total_tokens,
 provider_at,first_seen_at,source_generation,source_offset)
VALUES(?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(epoch_id,thread_id,response_id) DO UPDATE SET conflict=1 WHERE conflict=0 AND (
 cli_version IS NOT excluded.cli_version OR turn_id IS NOT excluded.turn_id OR root_turn_id IS NOT excluded.root_turn_id OR
 input_tokens IS NOT excluded.input_tokens OR cached_tokens IS NOT excluded.cached_tokens OR
 cache_write_tokens IS NOT excluded.cache_write_tokens OR output_tokens IS NOT excluded.output_tokens OR
 reasoning_tokens IS NOT excluded.reasoning_tokens OR total_tokens IS NOT excluded.total_tokens
);

-- name: ListGuardianRequests :many
SELECT * FROM guardian_requests WHERE epoch_id=? ORDER BY thread_id,response_id;

-- name: RefreshGuardianRequestCheckpoint :exec
UPDATE guardian_requests SET checkpoint_generation=sqlc.arg(source_generation),checkpoint_offset=sqlc.arg(source_offset)
WHERE epoch_id=sqlc.arg(epoch_id) AND thread_id=sqlc.arg(thread_id) AND response_id=sqlc.arg(response_id)
AND conflict=0 AND provider_at=sqlc.arg(provider_at)
AND (checkpoint_generation<>sqlc.arg(source_generation) OR checkpoint_offset<>sqlc.arg(source_offset));

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
AND NOT EXISTS(SELECT 1 FROM guardian_reviews r WHERE r.epoch_id=guardian_threads.epoch_id AND r.parent_thread_id=guardian_threads.parent_id)
AND NOT EXISTS(SELECT 1 FROM guardian_turn_models m WHERE m.epoch_id=guardian_threads.epoch_id AND m.thread_id=guardian_threads.thread_id);
-- name: ClearExpiredGuardianThreadMetadata :exec
UPDATE guardian_threads SET cli_version='' WHERE retention_at<=? AND cli_version<>'';
-- name: DeleteExpiredGuardianEpochs :exec
DELETE FROM guardian_epochs WHERE guardian_epochs.first_seen_at<=sqlc.arg(cutoff)
AND NOT EXISTS(SELECT 1 FROM guardian_reviews r WHERE r.epoch_id=guardian_epochs.id)
AND NOT EXISTS(SELECT 1 FROM guardian_threads t WHERE t.epoch_id=guardian_epochs.id)
AND NOT EXISTS(SELECT 1 FROM guardian_positions p WHERE p.epoch_id=guardian_epochs.id)
AND NOT EXISTS(SELECT 1 FROM guardian_anomaly_states a WHERE a.epoch_id=guardian_epochs.id);
-- name: DeleteExpiredGuardianPositions :exec
DELETE FROM guardian_positions WHERE updated_at<=sqlc.arg(cutoff)
AND NOT EXISTS(SELECT 1 FROM guardian_epochs e WHERE e.id=guardian_positions.epoch_id
 AND e.id IN (sqlc.slice('active_epoch_ids')));
-- name: DeleteExpiredGuardianAnomalies :exec
DELETE FROM guardian_anomalies WHERE first_seen_at<=?;
-- name: PurgeGuardianEpochs :exec
DELETE FROM guardian_epochs WHERE session_id=?;
-- name: PurgeGuardianAnomalies :exec
DELETE FROM guardian_anomalies WHERE session_id=?;

-- name: ListGuardianContinuityRequests :many
SELECT * FROM guardian_requests WHERE epoch_id=sqlc.arg(epoch_id) AND thread_id=sqlc.arg(thread_id)
AND ((checkpoint_generation=sqlc.arg(source_generation) AND checkpoint_offset>=sqlc.arg(start_offset) AND checkpoint_offset<sqlc.arg(end_offset) AND conflict=0)
 OR response_id IN (sqlc.slice('response_ids')))
ORDER BY checkpoint_offset,response_id
LIMIT 1025;

-- name: ListGuardianAnomalyStates :many
SELECT * FROM guardian_anomaly_states WHERE epoch_id=? AND file_identity=?;

-- name: PutGuardianAnomalyState :exec
INSERT INTO guardian_anomaly_states(epoch_id,file_identity,code,source_size,source_modified_at,source_generation,source_offset,first_seen_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(epoch_id,file_identity,code) DO UPDATE SET
 source_size=excluded.source_size,source_modified_at=excluded.source_modified_at,source_generation=excluded.source_generation,source_offset=excluded.source_offset;

-- name: DeleteGuardianAnomalyState :exec
DELETE FROM guardian_anomaly_states WHERE epoch_id=? AND file_identity=? AND code=?;

-- name: DeleteExpiredGuardianAnomalyStates :exec
DELETE FROM guardian_anomaly_states WHERE guardian_anomaly_states.first_seen_at<=sqlc.arg(cutoff)
OR EXISTS(SELECT 1 FROM guardian_epochs e JOIN guardian_anomalies a ON a.session_id=e.session_id
 WHERE e.id=guardian_anomaly_states.epoch_id AND a.code=guardian_anomaly_states.code AND a.first_seen_at<=sqlc.arg(cutoff));

-- name: GetGuardianEpoch :one
SELECT * FROM guardian_epochs WHERE binding_id=? AND account_id=? AND account_bound_at=?;

-- name: HasGuardianRequests :one
SELECT EXISTS(SELECT 1 FROM guardian_requests WHERE epoch_id=? AND thread_id=?);

-- name: GetGuardianReview :one
SELECT * FROM guardian_reviews WHERE epoch_id=? AND parent_thread_id=? AND review_id=?;
-- name: PutGuardianReview :exec
INSERT INTO guardian_reviews(epoch_id,parent_thread_id,review_id,parent_turn_id,target_item_id,action,status,decision_source,started_at,completed_at,first_seen_at,retention_at,saw_started,saw_completed,conflict)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(epoch_id,parent_thread_id,review_id) DO UPDATE SET
 status=excluded.status,decision_source=excluded.decision_source,started_at=excluded.started_at,
 completed_at=excluded.completed_at,retention_at=excluded.retention_at,
 saw_started=excluded.saw_started,saw_completed=excluded.saw_completed,conflict=excluded.conflict;
-- name: DeleteExpiredGuardianReviews :exec
DELETE FROM guardian_reviews WHERE retention_at<=?;

-- name: PageGuardianRequests :many
SELECT CAST(printf('%020d:%s:%s',r.epoch_id,r.thread_id,r.response_id) AS TEXT) AS sequence,r.* FROM guardian_requests r
JOIN guardian_epochs e ON e.id=r.epoch_id
WHERE e.session_id=sqlc.arg(session_id) AND r.provider_at>=sqlc.arg(since_at) AND r.provider_at<sqlc.arg(until_at)
 AND r.first_seen_at<sqlc.arg(until_at) AND r.provider_at>sqlc.arg(retention_floor) AND r.first_seen_at>sqlc.arg(retention_floor)
 AND CAST(printf('%020d:%s:%s',r.epoch_id,r.thread_id,r.response_id) AS TEXT)>CAST(sqlc.arg(after_sequence) AS TEXT)
ORDER BY sequence LIMIT 500;

-- name: PageGuardianReviews :many
SELECT CAST(printf('%020d:%s:%s',r.epoch_id,r.parent_thread_id,r.review_id) AS TEXT) AS sequence,r.* FROM guardian_reviews r
JOIN guardian_epochs e ON e.id=r.epoch_id
WHERE e.session_id=sqlc.arg(session_id) AND r.retention_at>=sqlc.arg(since_at) AND r.retention_at<sqlc.arg(until_at)
 AND r.first_seen_at<sqlc.arg(until_at) AND r.retention_at>sqlc.arg(retention_floor)
 AND CAST(printf('%020d:%s:%s',r.epoch_id,r.parent_thread_id,r.review_id) AS TEXT)>CAST(sqlc.arg(after_sequence) AS TEXT)
ORDER BY sequence LIMIT 500;
