-- Export reads intentionally bypass active-lineage and presentation filtering.
-- name: SelectTranscriptConversations :many
SELECT * FROM conversations c WHERE c.session_id = sqlc.arg(session_id)
 OR c.current_session_id = sqlc.arg(session_id)
 OR EXISTS (SELECT 1 FROM conversation_turns t WHERE t.conversation_id=c.id AND t.handled_by_session_id=sqlc.arg(session_id))
 OR EXISTS (SELECT 1 FROM conversation_branches b WHERE b.conversation_id=c.id AND b.session_id=sqlc.arg(session_id))
ORDER BY c.id;

-- name: SelectTranscriptTurns :many
SELECT * FROM conversation_turns WHERE handled_by_session_id=sqlc.arg(session_id) ORDER BY conversation_id, requested_at, id;

-- name: SelectTranscriptMessages :many
SELECT m.* FROM conversation_messages m
WHERE EXISTS (SELECT 1 FROM conversation_turns t WHERE t.id=m.turn_id AND t.handled_by_session_id=sqlc.arg(session_id))
 OR (m.turn_id IS NULL AND EXISTS (SELECT 1 FROM conversations c WHERE c.id=m.conversation_id AND c.session_id=sqlc.arg(session_id)))
ORDER BY m.conversation_id,m.sequence,m.id;

-- name: SelectTranscriptActivities :many
SELECT a.* FROM conversation_activities a
WHERE EXISTS (SELECT 1 FROM conversation_turns t WHERE t.id=a.turn_id AND t.handled_by_session_id=sqlc.arg(session_id))
ORDER BY a.conversation_id,a.sequence,a.id;

-- name: SelectTranscriptTerminalTurns :many
SELECT * FROM session_terminal_turns WHERE session_id=sqlc.arg(session_id) ORDER BY id;

-- name: SelectTranscriptNativeSessions :many
SELECT * FROM agent_native_sessions WHERE ao_session_id=sqlc.arg(session_id) ORDER BY created_at,id;

-- name: SelectTranscriptInterfaceTransitions :many
SELECT * FROM session_interface_transitions WHERE session_id=sqlc.arg(session_id) ORDER BY created_at,id;
