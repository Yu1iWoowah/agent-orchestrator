-- Counts and pages share branch visibility and canonical request identity.
-- name: SelectConversationAttentionPage :many
WITH RECURSIVE active_path(branch_id, max_sequence) AS (
 SELECT c0.active_branch_id, CAST(NULL AS INTEGER) FROM conversations c0 WHERE c0.id=sqlc.arg(conversation_id)
 UNION ALL
 SELECT b.parent_branch_id, CASE WHEN p.max_sequence IS NULL THEN b.fork_after_sequence WHEN b.fork_after_sequence<p.max_sequence THEN b.fork_after_sequence ELSE p.max_sequence END
 FROM active_path p JOIN conversation_branches b ON b.id=p.branch_id WHERE b.parent_branch_id IS NOT NULL
), candidates AS (
 SELECT a.id,
        CAST(CASE WHEN a.status='pending' AND a.request_id!='' AND a.branch_id=c.active_branch_id AND s.is_terminated=0
          AND (t.id IS NULL OR t.state IN ('queued','running')) THEN 1 ELSE 0 END AS INTEGER) AS actionable,
        ROW_NUMBER() OVER (PARTITION BY a.kind,CASE WHEN a.request_id!='' THEN 'request:'||a.request_id ELSE 'activity:'||a.id END ORDER BY a.sequence DESC) AS source_rank
 FROM conversation_activities a JOIN active_path p ON p.branch_id=a.branch_id
 JOIN conversations c ON c.id=a.conversation_id JOIN sessions s ON s.id=c.current_session_id
 LEFT JOIN conversation_turns t ON t.id=a.turn_id AND t.conversation_id=a.conversation_id
 WHERE a.conversation_id=sqlc.arg(conversation_id) AND a.kind IN ('approval','user_input')
 AND a.sequence>sqlc.arg(visible_after_sequence)
 AND (p.max_sequence IS NULL OR a.sequence<=p.max_sequence)
 AND (t.id IS NULL OR (t.rolled_back_at IS NULL AND t.state!='cancelled'))
)
SELECT sqlc.embed(conversation_activities), candidates.actionable,
       CAST(COALESCE(r.last_seen_revision,-1) AS INTEGER) AS last_seen_revision
FROM conversation_activities JOIN candidates ON candidates.id=conversation_activities.id
LEFT JOIN attention_reads r ON r.conversation_id=conversation_activities.conversation_id AND r.activity_id=conversation_activities.id AND r.operator_scope='local'
WHERE candidates.source_rank=1
AND (CAST(sqlc.arg(page_state) AS TEXT)='all' OR (CAST(sqlc.arg(page_state) AS TEXT)='open' AND candidates.actionable=1) OR (CAST(sqlc.arg(page_state) AS TEXT)='history' AND candidates.actionable=0))
AND conversation_activities.sequence<sqlc.arg(before_sequence)
AND (CAST(sqlc.arg(activity_id) AS TEXT)='' OR conversation_activities.id=sqlc.arg(activity_id))
ORDER BY conversation_activities.sequence DESC LIMIT sqlc.arg(page_limit);

-- name: CountConversationAttention :one
WITH RECURSIVE active_path(branch_id, max_sequence) AS (
 SELECT c0.active_branch_id, CAST(NULL AS INTEGER) FROM conversations c0 WHERE c0.id=sqlc.arg(conversation_id)
 UNION ALL
 SELECT b.parent_branch_id, CASE WHEN p.max_sequence IS NULL THEN b.fork_after_sequence WHEN b.fork_after_sequence<p.max_sequence THEN b.fork_after_sequence ELSE p.max_sequence END
 FROM active_path p JOIN conversation_branches b ON b.id=p.branch_id WHERE b.parent_branch_id IS NOT NULL
), candidates AS (
 SELECT a.id,
        CAST(CASE WHEN a.status='pending' AND a.request_id!='' AND a.branch_id=c.active_branch_id AND s.is_terminated=0
          AND (t.id IS NULL OR t.state IN ('queued','running')) THEN 1 ELSE 0 END AS INTEGER) AS actionable,
        ROW_NUMBER() OVER (PARTITION BY a.kind,CASE WHEN a.request_id!='' THEN 'request:'||a.request_id ELSE 'activity:'||a.id END ORDER BY a.sequence DESC) AS source_rank
 FROM conversation_activities a JOIN active_path p ON p.branch_id=a.branch_id
 JOIN conversations c ON c.id=a.conversation_id JOIN sessions s ON s.id=c.current_session_id
 LEFT JOIN conversation_turns t ON t.id=a.turn_id AND t.conversation_id=a.conversation_id
 WHERE a.conversation_id=sqlc.arg(conversation_id) AND a.kind IN ('approval','user_input')
 AND a.sequence>sqlc.arg(visible_after_sequence)
 AND (p.max_sequence IS NULL OR a.sequence<=p.max_sequence)
 AND (t.id IS NULL OR (t.rolled_back_at IS NULL AND t.state!='cancelled'))
)
SELECT CAST(COALESCE(SUM(candidates.actionable),0) AS INTEGER) AS open_count,
       CAST(COALESCE(SUM(CASE WHEN candidates.actionable=1 AND COALESCE(r.last_seen_revision,-1)<a.revision THEN 1 ELSE 0 END),0) AS INTEGER) AS unread_count,
       CAST(COUNT(*)-COALESCE(SUM(candidates.actionable),0) AS INTEGER) AS history_count
FROM candidates JOIN conversation_activities a ON a.id=candidates.id
LEFT JOIN attention_reads r ON r.conversation_id=a.conversation_id AND r.activity_id=a.id AND r.operator_scope='local'
WHERE candidates.source_rank=1;

-- name: MarkConversationAttentionRead :exec
INSERT INTO attention_reads(conversation_id,activity_id,last_seen_revision,seen_at)
VALUES (sqlc.arg(conversation_id),sqlc.arg(activity_id),sqlc.arg(revision),sqlc.arg(seen_at))
ON CONFLICT(operator_scope,conversation_id,activity_id) DO UPDATE SET last_seen_revision=excluded.last_seen_revision,seen_at=excluded.seen_at
WHERE attention_reads.last_seen_revision<excluded.last_seen_revision;
