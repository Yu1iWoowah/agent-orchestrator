-- name: GetAttentionReportQuestion :one
SELECT question_id FROM attention_report_questions WHERE report_id=?;
-- name: GetQuestionSourceReport :one
SELECT report_id FROM attention_report_questions WHERE question_id=?;
-- name: InsertAttentionReportQuestion :exec
INSERT INTO attention_report_questions(report_id,question_id,forwarded_by,created_at) VALUES(?,?,?,?);
-- name: ActiveAttentionOrchestrator :one
SELECT id FROM sessions WHERE project_id=? AND kind='orchestrator' AND is_terminated=0 AND activity_state!='exited'
ORDER BY created_at DESC,id DESC LIMIT 1;
-- name: ProjectAttentionQuestionSources :many
SELECT id,origin_session_id,version,seen_version,CAST(json_extract(payload_json,'$.title') AS TEXT) AS title,created_at FROM attention_questions
WHERE project_id=sqlc.arg(project_id) AND audience='user' AND state='open' AND (expires_at IS NULL OR expires_at>sqlc.arg(at));
-- name: ProjectAttentionReportSources :many
SELECT r.*,CAST(seen.report_id IS NOT NULL AS INTEGER) AS is_read,COALESCE(link.question_id,'') AS question_id
FROM reports r JOIN sessions s ON s.id=r.session_id
LEFT JOIN attention_report_reads seen ON seen.report_id=r.id
LEFT JOIN attention_report_questions link ON link.report_id=r.id
LEFT JOIN attention_questions question ON question.id=link.question_id
WHERE r.project_id=sqlc.arg(project_id) AND s.is_terminated=0
AND (
 -- A canonical user question owns its lifecycle, independently of later reports.
 (link.question_id IS NOT NULL AND question.state='open'
  AND (question.expires_at IS NULL OR question.expires_at>sqlc.arg(at)))
 OR
 -- Progress alone is not a replacement or a closing fact.
 (link.question_id IS NULL AND r.state IN ('needs_input','stuck')
  AND NOT EXISTS(SELECT 1 FROM reports newer WHERE newer.session_id=r.session_id
   AND newer.state IN ('needs_input','stuck','done')
   AND (newer.created_at>r.created_at OR (newer.created_at=r.created_at AND newer.id>r.id))))
);
-- name: ReadAttentionReport :exec
INSERT INTO attention_report_reads(report_id,seen_at) VALUES(?,?) ON CONFLICT(report_id) DO NOTHING;

-- name: AttentionNotificationWasSent :one
SELECT EXISTS(SELECT 1 FROM attention_notification_receipts WHERE source_key=?);
-- name: InsertAttentionNotificationReceipt :exec
INSERT INTO attention_notification_receipts(source_key,project_id) VALUES(?,?);
-- name: ListProjectAttentionNotifications :many
SELECT * FROM notifications WHERE project_id=? AND source_key LIKE 'attention:%' AND resolved_at IS NULL;
-- name: ResolveAttentionNotification :one
UPDATE notifications SET resolved_at=? WHERE id=? RETURNING *;
-- name: ReadAttentionNotification :one
UPDATE notifications SET status='read' WHERE id=? AND status='unread' RETURNING *;

-- name: ProjectAttentionRevisions :many
SELECT p.id,CAST(COALESCE(r.revision,0) AS INTEGER) AS revision
FROM projects p LEFT JOIN project_attention_revisions r ON r.project_id=p.id
WHERE p.archived_at IS NULL;
