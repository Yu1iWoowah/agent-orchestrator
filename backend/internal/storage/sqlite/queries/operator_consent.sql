-- name: GetOperatorConfirmationByKey :one
SELECT * FROM operator_confirmation_requests WHERE action_key=?;
-- name: GetOperatorConfirmationByRef :one
SELECT * FROM operator_confirmation_requests WHERE request_ref=?;
-- name: InsertOperatorConfirmation :exec
INSERT INTO operator_confirmation_requests(request_ref,action_key,project_id,purpose,plan_json,plan_digest,daemon_epoch,valid_until_ms,created_at_ms)
VALUES(?,?,?,?,?,?,?,?,?);
-- name: RearmOperatorConfirmation :exec
UPDATE operator_confirmation_requests SET daemon_epoch=? WHERE request_ref=? AND revoked_at_ms IS NULL;
-- name: RevokeOperatorConfirmation :exec
UPDATE operator_confirmation_requests SET revoked_at_ms=? WHERE request_ref=? AND revoked_at_ms IS NULL;
-- name: GetLiveOperatorConsent :one
SELECT * FROM operator_consents WHERE request_ref=? AND invalidated_at_ms IS NULL;
-- name: GetOperatorConsentByRef :one
SELECT * FROM operator_consents WHERE consent_ref=?;
-- name: InvalidateUnusedOperatorConsents :exec
UPDATE operator_consents SET invalidated_at_ms=? WHERE request_ref=? AND consumed_at_ms IS NULL AND invalidated_at_ms IS NULL;
-- name: ConsumeOperatorConsent :execrows
UPDATE operator_consents SET consumed_at_ms=?,reservation_ref=?
WHERE consent_ref=? AND consumed_at_ms IS NULL AND invalidated_at_ms IS NULL;
