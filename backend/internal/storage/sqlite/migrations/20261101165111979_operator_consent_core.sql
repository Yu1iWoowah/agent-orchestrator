-- +goose Up
CREATE TABLE operator_confirmation_requests (
 request_ref TEXT PRIMARY KEY,
 action_key TEXT NOT NULL UNIQUE,
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 purpose TEXT NOT NULL,
 plan_json TEXT NOT NULL CHECK(json_valid(plan_json)),
 plan_digest TEXT NOT NULL CHECK(length(plan_digest)=64),
 daemon_epoch TEXT NOT NULL,
 valid_until_ms INTEGER NOT NULL,
 revoked_at_ms INTEGER,
 created_at_ms INTEGER NOT NULL
);
CREATE TABLE operator_consents (
 consent_ref TEXT PRIMARY KEY,
 request_ref TEXT NOT NULL REFERENCES operator_confirmation_requests(request_ref) ON DELETE CASCADE,
 plan_digest TEXT NOT NULL CHECK(length(plan_digest)=64),
 daemon_epoch TEXT NOT NULL,
 issuer_epoch TEXT NOT NULL,
 minted_at_ms INTEGER NOT NULL,
 expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms>minted_at_ms AND expires_at_ms<=minted_at_ms+300000),
 invalidated_at_ms INTEGER,
 consumed_at_ms INTEGER,
 reservation_ref TEXT NOT NULL DEFAULT '',
 CHECK((consumed_at_ms IS NULL AND reservation_ref='') OR (consumed_at_ms IS NOT NULL AND reservation_ref<>'')),
 CHECK(consumed_at_ms IS NULL OR invalidated_at_ms IS NULL)
);
CREATE UNIQUE INDEX operator_consents_live_request ON operator_consents(request_ref) WHERE invalidated_at_ms IS NULL;
-- No plan JSON, digests, consent refs, issuer material or provider data in CDC.
-- +goose StatementBegin
CREATE TRIGGER operator_request_created AFTER INSERT ON operator_confirmation_requests BEGIN
 INSERT INTO change_log(project_id,event_type,payload) VALUES (NEW.project_id,'session_updated',json_object('operatorRequestRef',NEW.request_ref));
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER operator_request_updated AFTER UPDATE ON operator_confirmation_requests BEGIN
 INSERT INTO change_log(project_id,event_type,payload) VALUES (NEW.project_id,'session_updated',json_object('operatorRequestRef',NEW.request_ref));
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER operator_consent_created AFTER INSERT ON operator_consents BEGIN
 INSERT INTO change_log(project_id,event_type,payload) SELECT project_id,'session_updated',json_object('operatorRequestRef',NEW.request_ref) FROM operator_confirmation_requests WHERE request_ref=NEW.request_ref;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER operator_consent_updated AFTER UPDATE ON operator_consents BEGIN
 INSERT INTO change_log(project_id,event_type,payload) SELECT project_id,'session_updated',json_object('operatorRequestRef',NEW.request_ref) FROM operator_confirmation_requests WHERE request_ref=NEW.request_ref;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER operator_consent_updated;
DROP TRIGGER operator_consent_created;
DROP TRIGGER operator_request_updated;
DROP TRIGGER operator_request_created;
DROP TABLE operator_consents;
DROP TABLE operator_confirmation_requests;
