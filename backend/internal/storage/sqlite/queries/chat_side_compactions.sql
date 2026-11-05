-- name: InsertSideCompaction :exec
INSERT INTO chat_side_compactions(id,side_id,native_home,native_id,epoch,state,record_json)
VALUES(?,?,?,?,?,'pending',?);

-- name: ListSideCompactions :many
SELECT c.*,COALESCE(s.content_json,'') AS content_json FROM chat_side_compactions c
LEFT JOIN chat_side_contents s ON s.side_id=c.side_id
WHERE sqlc.arg(unresolved_only)=0 OR (c.closed=0 AND c.state IN ('pending','unknown'));

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

-- name: DeleteSettledSideContent :exec
DELETE FROM chat_side_contents WHERE chat_side_contents.side_id=sqlc.arg(side_id) AND NOT EXISTS
(SELECT 1 FROM chat_side_compactions c WHERE c.side_id=chat_side_contents.side_id AND c.closed=0 AND c.state IN ('pending','unknown'));
