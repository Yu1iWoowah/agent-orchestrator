-- +goose Up
-- Historical recovery and lifecycle owners share these retirement proofs.
-- New intents have an explicit state; an empty terminal receipt on a live
-- session predates receipts and means the old daemon already confirmed stop.
CREATE VIEW account_transition_recovery_evidence AS
SELECT t.id,
 CAST(CASE WHEN
   (s.is_terminated=0 AND t.target_runtime_state='')
   OR (s.is_terminated=0 AND t.target_runtime_state='started'
       AND t.target_runtime_launch_id<>'' AND s.runtime_launch_id<>''
       AND s.runtime_launch_id<>t.target_runtime_launch_id)
   OR EXISTS (
     SELECT 1 FROM session_interface_transitions AS successor
     WHERE successor.session_id=t.session_id AND successor.rowid>t.rowid
       AND (successor.phase='completed'
         OR (successor.target_account_id<>'' AND EXISTS (
           SELECT 1 FROM session_agent_accounts AS b
           WHERE b.session_id=t.session_id AND b.account_id=successor.target_account_id
             AND b.bound_at=(SELECT MAX(c.bound_at) FROM session_agent_accounts AS c
                             WHERE c.session_id=b.session_id AND c.provider=b.provider)))))
   THEN 1 ELSE 0 END AS INTEGER) AS retire_without_stop,
 CAST(s.is_terminated=0 AND EXISTS (
   SELECT 1 FROM session_agent_accounts AS b
   WHERE b.session_id=t.session_id AND b.account_id=t.target_account_id
     AND b.bound_at=(SELECT MAX(c.bound_at) FROM session_agent_accounts AS c
                     WHERE c.session_id=b.session_id AND c.provider=b.provider))
   AS INTEGER) AS current_target
FROM session_interface_transitions AS t JOIN sessions AS s ON s.id=t.session_id
WHERE t.phase='recovery_required' AND t.target_account_id<>''
  AND t.target_runtime_state NOT IN ('not_started','stopped');

-- +goose Down
DROP VIEW account_transition_recovery_evidence;
