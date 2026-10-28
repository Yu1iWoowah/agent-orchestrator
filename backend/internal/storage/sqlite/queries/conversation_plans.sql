-- Latest structured plan activities, bounded independently of chat pages.
-- name: SelectConversationPlanActivities :many
WITH RECURSIVE active_path(branch_id, max_sequence) AS (
    SELECT active_branch_id, CAST(NULL AS INTEGER)
    FROM conversations WHERE id = sqlc.arg(conversation_id)
    UNION ALL
    SELECT branch.parent_branch_id,
           CASE
               WHEN path.max_sequence IS NULL THEN branch.fork_after_sequence
               WHEN branch.fork_after_sequence < path.max_sequence THEN branch.fork_after_sequence
               ELSE path.max_sequence
           END
    FROM active_path AS path
    JOIN conversation_branches AS branch ON branch.id = path.branch_id
    WHERE branch.parent_branch_id IS NOT NULL
)
SELECT activity.id AS source_activity_id,
       CAST(COALESCE(activity.turn_id, '') AS TEXT) AS turn_id,
       activity.branch_id, activity.sequence, activity.revision, activity.updated_at,
       CAST(COALESCE(NULLIF(turn.plan_json, ''), activity.detail_json) AS TEXT) AS plan_json
FROM conversation_activities AS activity
JOIN active_path AS path ON path.branch_id = activity.branch_id
LEFT JOIN conversation_turns AS turn
  ON turn.id = activity.turn_id AND turn.conversation_id = activity.conversation_id
WHERE activity.conversation_id = sqlc.arg(conversation_id)
  AND activity.kind = 'plan'
  AND NOT EXISTS (SELECT 1 FROM conversation_plan_versions v WHERE v.conversation_id=activity.conversation_id AND v.source_activity_id=activity.id)
  AND (path.max_sequence IS NULL OR activity.sequence <= path.max_sequence)
  AND (turn.id IS NULL OR (turn.rolled_back_at IS NULL AND turn.state != 'cancelled'))
  AND activity.sequence > sqlc.arg(visible_after_sequence)
  AND activity.sequence < sqlc.arg(before_sequence)
ORDER BY activity.sequence DESC
LIMIT sqlc.arg(page_limit);

-- A legacy turn falls back only when no plan activity exists at all. A newer
-- activity beyond the fork cutoff must not leak back through the prompt.
-- name: SelectLegacyConversationPlans :many
WITH RECURSIVE active_path(branch_id, max_sequence) AS (
    SELECT active_branch_id, CAST(NULL AS INTEGER)
    FROM conversations WHERE id = sqlc.arg(conversation_id)
    UNION ALL
    SELECT branch.parent_branch_id,
           CASE
               WHEN path.max_sequence IS NULL THEN branch.fork_after_sequence
               WHEN branch.fork_after_sequence < path.max_sequence THEN branch.fork_after_sequence
               ELSE path.max_sequence
           END
    FROM active_path AS path
    JOIN conversation_branches AS branch ON branch.id = path.branch_id
    WHERE branch.parent_branch_id IS NOT NULL
)
SELECT turn.id AS turn_id, turn.branch_id,
       CAST(MIN(message.sequence) AS INTEGER) AS sequence,
       turn.requested_at AS updated_at, turn.plan_json
FROM conversation_turns AS turn
JOIN active_path AS path ON path.branch_id = turn.branch_id
JOIN conversation_messages AS message
  ON message.turn_id = turn.id AND message.conversation_id = turn.conversation_id
WHERE turn.conversation_id = sqlc.arg(conversation_id)
  AND turn.plan_json != '' AND NOT EXISTS (SELECT 1 FROM conversation_plan_versions v WHERE v.conversation_id=turn.conversation_id AND v.source_activity_id='legacy-turn-'||turn.id) AND turn.rolled_back_at IS NULL AND turn.state != 'cancelled'
  AND message.sequence < sqlc.arg(before_sequence)
  AND (path.max_sequence IS NULL OR message.sequence <= path.max_sequence)
  AND NOT EXISTS (
      SELECT 1 FROM conversation_activities AS activity
      WHERE activity.conversation_id = turn.conversation_id
        AND activity.turn_id = turn.id AND activity.kind = 'plan'
  )
GROUP BY turn.id
HAVING MIN(message.sequence) > sqlc.arg(visible_after_sequence)
ORDER BY MIN(message.sequence) DESC
LIMIT sqlc.arg(page_limit);

-- name: InsertLegacyPlanVersion :exec
INSERT OR IGNORE INTO conversation_plan_versions(conversation_id,branch_id,source_activity_id,turn_id,accepted_sequence,revision,plan_json,updated_at)
SELECT old.conversation_id,old.branch_id,old.id,COALESCE(old.turn_id,''),old.sequence,old.revision+1,old.detail_json,old.updated_at
FROM conversation_activities AS old WHERE old.id=sqlc.arg(activity_id) AND old.kind='plan' AND json_valid(old.detail_json)
AND NOT EXISTS (SELECT 1 FROM conversation_plan_versions v WHERE v.conversation_id=old.conversation_id AND v.source_activity_id=old.id);

-- name: BackfillLegacyPlanVersion :exec
INSERT OR IGNORE INTO conversation_plan_versions(conversation_id,branch_id,source_activity_id,turn_id,accepted_sequence,revision,plan_json,updated_at)
SELECT old.conversation_id,old.branch_id,old.id,COALESCE(old.turn_id,''),old.sequence,old.revision+1,COALESCE(NULLIF(t.plan_json,''),old.detail_json),old.updated_at
FROM conversation_activities AS old LEFT JOIN conversation_turns t ON t.id=old.turn_id AND t.conversation_id=old.conversation_id WHERE old.id=sqlc.arg(activity_id) AND old.kind='plan' AND json_valid(old.detail_json)
AND NOT EXISTS (SELECT 1 FROM conversation_plan_versions v WHERE v.conversation_id=old.conversation_id AND v.source_activity_id=old.id);

-- name: ProjectConversationPlan :exec
INSERT INTO conversation_plan_heads(conversation_id,branch_id,source_activity_id,turn_id,accepted_sequence,revision,plan_json,updated_at)
SELECT a.conversation_id,c.active_branch_id,a.id,COALESCE(a.turn_id,''),sqlc.arg(accepted_sequence),1,COALESCE(NULLIF(t.plan_json,''),a.detail_json),sqlc.arg(updated_at)
FROM conversation_activities a JOIN conversations c ON c.id=a.conversation_id
LEFT JOIN conversation_turns t ON t.id=a.turn_id AND t.conversation_id=a.conversation_id
WHERE a.conversation_id=sqlc.arg(conversation_id) AND a.id=sqlc.arg(activity_id)
ON CONFLICT(conversation_id,branch_id) DO UPDATE SET source_activity_id=excluded.source_activity_id,turn_id=excluded.turn_id,
 accepted_sequence=excluded.accepted_sequence,revision=conversation_plan_heads.revision+1,plan_json=excluded.plan_json,updated_at=excluded.updated_at;

-- name: ArchiveConversationPlanHead :exec
INSERT INTO conversation_plan_versions(conversation_id,branch_id,source_activity_id,turn_id,accepted_sequence,revision,plan_json,updated_at)
SELECT h.conversation_id,h.branch_id,h.source_activity_id,h.turn_id,h.accepted_sequence,h.revision,h.plan_json,h.updated_at FROM conversation_plan_heads AS h WHERE h.conversation_id=sqlc.arg(conversation_id) AND h.accepted_sequence=sqlc.arg(accepted_sequence);

-- name: SelectConversationPlanVersions :many
WITH RECURSIVE active_path(branch_id,max_sequence) AS (
 SELECT c.active_branch_id,CAST(NULL AS INTEGER) FROM conversations AS c WHERE c.id=sqlc.arg(conversation_id)
 UNION ALL
 SELECT b.parent_branch_id,CASE WHEN p.max_sequence IS NULL THEN b.fork_after_sequence WHEN b.fork_after_sequence<p.max_sequence THEN b.fork_after_sequence ELSE p.max_sequence END
 FROM active_path p JOIN conversation_branches b ON b.id=p.branch_id WHERE b.parent_branch_id IS NOT NULL
), visible AS (
 SELECT v.*, ROW_NUMBER() OVER (PARTITION BY CASE WHEN v.turn_id != '' THEN 'turn:' || v.turn_id ELSE 'activity:' || v.source_activity_id END ORDER BY v.accepted_sequence DESC) AS source_rank
 FROM conversation_plan_versions v JOIN active_path p ON p.branch_id=v.branch_id
 LEFT JOIN conversation_turns t ON t.id=v.turn_id AND t.conversation_id=v.conversation_id
 WHERE v.conversation_id=sqlc.arg(conversation_id) AND (p.max_sequence IS NULL OR v.accepted_sequence<=p.max_sequence)
 AND (t.id IS NULL OR (t.rolled_back_at IS NULL AND t.state!='cancelled'))
 AND v.accepted_sequence>sqlc.arg(visible_after_sequence)
)
SELECT conversation_id,branch_id,source_activity_id,turn_id,accepted_sequence,revision,plan_json,updated_at
FROM visible WHERE source_rank=1 AND accepted_sequence<sqlc.arg(before_sequence)
ORDER BY accepted_sequence DESC LIMIT sqlc.arg(page_limit);

-- name: InsertLegacyTurnPlanVersion :exec
INSERT OR IGNORE INTO conversation_plan_versions(conversation_id,branch_id,source_activity_id,turn_id,accepted_sequence,revision,plan_json,updated_at)
SELECT t.conversation_id,t.branch_id,'legacy-turn-'||t.id,t.id,MIN(m.sequence),1,t.plan_json,t.requested_at
FROM conversation_turns t JOIN conversation_messages m ON m.turn_id=t.id AND m.conversation_id=t.conversation_id
WHERE t.id=sqlc.arg(turn_id) AND t.plan_json!=''
AND NOT EXISTS (SELECT 1 FROM conversation_activities a WHERE a.conversation_id=t.conversation_id AND a.turn_id=t.id AND a.kind='plan')
GROUP BY t.id;

-- name: BackfillMissingConversationPlanHeads :exec
INSERT OR IGNORE INTO conversation_plan_heads(conversation_id,branch_id,source_activity_id,turn_id,accepted_sequence,revision,plan_json,updated_at)
SELECT v.conversation_id,v.branch_id,v.source_activity_id,v.turn_id,v.accepted_sequence,v.revision,v.plan_json,v.updated_at
FROM conversation_plan_versions v WHERE v.conversation_id=sqlc.arg(conversation_id) ORDER BY v.accepted_sequence DESC;

-- Completion changes display state, never which source was accepted last.
-- name: FinalizeConversationPlanHead :exec
UPDATE conversation_plan_heads SET plan_json=sqlc.arg(plan_json),updated_at=sqlc.arg(updated_at),revision=revision+1
WHERE conversation_plan_heads.conversation_id=sqlc.arg(conversation_id) AND conversation_plan_heads.turn_id=sqlc.arg(turn_id)
AND conversation_plan_heads.branch_id=(SELECT c.active_branch_id FROM conversations c WHERE c.id=sqlc.arg(conversation_id));

-- name: FinalizeConversationPlanVersion :exec
UPDATE conversation_plan_versions SET plan_json=sqlc.arg(plan_json),updated_at=sqlc.arg(updated_at),revision=revision+1
WHERE conversation_plan_versions.conversation_id=sqlc.arg(conversation_id) AND conversation_plan_versions.turn_id=sqlc.arg(turn_id)
AND conversation_plan_versions.branch_id=(SELECT c.active_branch_id FROM conversations c WHERE c.id=sqlc.arg(conversation_id))
AND conversation_plan_versions.accepted_sequence=(SELECT MAX(v.accepted_sequence) FROM conversation_plan_versions v WHERE v.conversation_id=sqlc.arg(conversation_id) AND v.turn_id=sqlc.arg(turn_id));
