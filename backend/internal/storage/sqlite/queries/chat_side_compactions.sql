-- name: InsertSideCompaction :exec
INSERT INTO chat_side_compactions(id,side_id,native_home,native_id,epoch,state,record_json)
VALUES(?,?,?,?,?,'pending',?);

-- name: ListSideCompactions :many
SELECT c.*,COALESCE(s.content_json,'') AS content_json FROM chat_side_compactions c
LEFT JOIN chat_side_contents s ON s.side_id=c.side_id
WHERE sqlc.arg(unresolved_only)=0 OR (c.closed=0 AND c.state IN ('pending','unknown'));

-- name: ListSideCompactionRecovery :many
SELECT side_id, state, closed,
       CAST(COALESCE(json_extract(record_json,'$.ProviderHostID'),'') AS TEXT) AS provider_host_id
FROM chat_side_compactions
GROUP BY side_id, state, closed, provider_host_id
ORDER BY MAX(rowid);

-- name: SideCompactionContent :one
SELECT CAST(COALESCE((SELECT content_json FROM chat_side_contents WHERE side_id=?),'') AS TEXT);

-- name: SideWithUnresolvedNativeCompaction :one
SELECT CAST(COALESCE((SELECT side_id FROM chat_side_compactions
WHERE native_home=? AND native_id=? AND closed=0 AND state IN ('pending','unknown') LIMIT 1),'') AS TEXT);

-- name: CloseSideCompactions :exec
UPDATE chat_side_compactions SET closed=1 WHERE side_id=? AND closed=0;

-- name: HasUnresolvedSideCompaction :one
SELECT CAST(EXISTS(SELECT 1 FROM chat_side_compactions
WHERE side_id=? AND closed=0 AND state IN ('pending','unknown')) AS INTEGER);

-- name: UnresolvedSideForClose :one
SELECT native_id,
       CAST(COALESCE(json_extract(record_json,'$.SessionID'),'') AS TEXT) AS session_id,
       CAST(COALESCE(json_extract(record_json,'$.ProviderHostID'),'') AS TEXT) AS provider_host_id,
       CAST(COALESCE(json_extract(record_json,'$.Provider'),'') AS TEXT) AS provider
FROM chat_side_compactions
WHERE side_id=? AND closed=0 AND state IN ('pending','unknown')
LIMIT 1;

-- name: GetSideCompaction :one
SELECT * FROM chat_side_compactions WHERE id=?;

-- name: PutSideCompactionContent :exec
INSERT INTO chat_side_contents(side_id,content_json) VALUES(?,?)
ON CONFLICT(side_id) DO UPDATE SET content_json=excluded.content_json;

-- name: UpdateSideCompactionContent :exec
UPDATE chat_side_contents SET content_json=sqlc.arg(content_json) WHERE chat_side_contents.side_id=sqlc.arg(side_id) AND EXISTS
(SELECT 1 FROM chat_side_compactions c WHERE c.side_id=chat_side_contents.side_id AND c.closed=0 AND c.state IN ('pending','unknown'));

-- name: UpdateSideCompaction :exec
UPDATE chat_side_compactions SET state=?,historical=?,closed=? WHERE id=?;

-- name: MarkSideCompactionHistorical :exec
UPDATE chat_side_compactions SET state='unknown',historical=1,record_json=? WHERE id=?;

-- name: DeleteSettledSideContent :exec
DELETE FROM chat_side_contents WHERE chat_side_contents.side_id=sqlc.arg(side_id) AND NOT EXISTS
(SELECT 1 FROM chat_side_compactions c WHERE c.side_id=chat_side_contents.side_id AND c.closed=0 AND c.state IN ('pending','unknown'));
