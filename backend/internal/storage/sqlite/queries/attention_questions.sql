-- name: InsertAttentionQuestion :one
INSERT INTO attention_questions(id,project_id,origin_session_id,origin_harness,client_key,audience,kind,payload_json,create_fingerprint,supersedes_id,created_at,updated_at,expires_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING *;

-- name: GetAttentionQuestion :one
SELECT * FROM attention_questions WHERE origin_session_id=? AND id=?;

-- name: FindAttentionQuestionKey :one
SELECT * FROM attention_questions WHERE origin_session_id=? AND client_key=?;

-- name: ListAttentionQuestions :many
SELECT * FROM attention_questions WHERE origin_session_id=sqlc.arg(origin_session_id)
AND (CAST(sqlc.arg(page_state) AS TEXT)='all' OR (CAST(sqlc.arg(page_state) AS TEXT)='open' AND state='open') OR (CAST(sqlc.arg(page_state) AS TEXT)='history' AND state!='open'))
AND sequence<sqlc.arg(before_sequence) ORDER BY sequence DESC LIMIT sqlc.arg(page_limit);

-- name: CountAttentionQuestions :one
SELECT CAST(COALESCE(SUM(state='open'),0) AS INTEGER) AS open_count,
CAST(COALESCE(SUM(state='open' AND seen_version<version),0) AS INTEGER) AS unread_count,
CAST(COALESCE(SUM(state!='open'),0) AS INTEGER) AS history_count
FROM attention_questions WHERE origin_session_id=?;

-- name: CloseAttentionQuestion :execrows
UPDATE attention_questions SET state=sqlc.arg(state),closed_at=sqlc.arg(at),updated_at=sqlc.arg(at),closed_by=sqlc.arg(actor),close_reason=sqlc.arg(reason)
WHERE origin_session_id=sqlc.arg(origin_session_id) AND id=sqlc.arg(id) AND version=sqlc.arg(expected_version) AND state='open';

-- name: ReviseAttentionQuestion :execrows
UPDATE attention_questions SET payload_json=sqlc.arg(payload_json),version=version+1,updated_at=sqlc.arg(at)
WHERE origin_session_id=sqlc.arg(origin_session_id) AND id=sqlc.arg(id) AND version=sqlc.arg(expected_version) AND state='open' AND version<9007199254740991;

-- name: ReadAttentionQuestion :execrows
UPDATE attention_questions SET seen_version=version,seen_at=sqlc.arg(at)
WHERE origin_session_id=sqlc.arg(origin_session_id) AND id=sqlc.arg(id) AND version=sqlc.arg(expected_version);

-- name: ExpireAttentionQuestions :execrows
UPDATE attention_questions SET state='expired',closed_at=sqlc.arg(at),updated_at=sqlc.arg(at),closed_by='system',close_reason='deadline'
WHERE origin_session_id=sqlc.arg(origin_session_id) AND state='open' AND expires_at IS NOT NULL AND expires_at<=sqlc.arg(at);

-- name: AttentionQuestionsNeedExpiration :one
SELECT EXISTS(SELECT 1 FROM attention_questions WHERE origin_session_id=sqlc.arg(origin_session_id) AND state='open' AND expires_at IS NOT NULL AND expires_at<=sqlc.arg(at));

-- name: AttentionQuestionExpirationSessions :many
SELECT DISTINCT origin_session_id FROM attention_questions WHERE state='open' AND expires_at IS NOT NULL AND expires_at<=?;

-- name: InsertAttentionAnswer :one
INSERT INTO attention_answers(question_id,client_answer_id,question_version,answers_json,fingerprint,message,saved_at)
VALUES(?,?,?,?,?,?,?) RETURNING *;

-- name: GetAttentionAnswer :one
SELECT * FROM attention_answers WHERE question_id=?;

-- name: PendingAttentionAnswers :many
SELECT sqlc.embed(attention_answers),q.origin_session_id,q.origin_harness FROM attention_answers JOIN attention_questions q ON q.id=attention_answers.question_id
WHERE attention_answers.delivery_state='queued' ORDER BY delivery_attempts,saved_at,question_id LIMIT ?;

-- name: RecordAttentionAnswerDelivery :execrows
UPDATE attention_answers SET delivery_state=sqlc.arg(delivery_state),delivered_at=sqlc.narg(delivered_at),last_error=sqlc.arg(last_error),delivery_attempts=delivery_attempts+1
WHERE question_id=sqlc.arg(question_id) AND delivery_state='queued';

-- Human answer provenance is recorded only after semantic acceptance, without
-- destroying the exact delivery checkpoint used to deduplicate TUI retries.
-- name: RecordAttentionAnswerHumanDirection :exec
UPDATE sessions SET
latest_user_prompt=(SELECT substr(attention_answers.message,1,4096) FROM attention_answers WHERE attention_answers.question_id=sqlc.arg(question_id)),
latest_user_prompt_at=(SELECT attention_answers.saved_at FROM attention_answers WHERE attention_answers.question_id=sqlc.arg(question_id)),
latest_assistant_update='',
updated_at=MAX(updated_at,(SELECT attention_answers.saved_at FROM attention_answers WHERE attention_answers.question_id=sqlc.arg(question_id)))
WHERE sessions.id=(SELECT attention_questions.origin_session_id FROM attention_questions WHERE attention_questions.id=sqlc.arg(question_id))
AND is_terminated=0 AND (latest_user_prompt_at IS NULL OR latest_user_prompt_at<=(SELECT attention_answers.saved_at FROM attention_answers WHERE attention_answers.question_id=sqlc.arg(question_id)));
