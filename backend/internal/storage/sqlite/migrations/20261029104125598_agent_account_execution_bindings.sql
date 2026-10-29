-- +goose Up
ALTER TABLE automations ADD COLUMN agent_account_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_agent_accounts ADD COLUMN resolution_source TEXT NOT NULL DEFAULT 'legacy'
 CHECK (resolution_source IN ('explicit','role','project','global','device','legacy'));

CREATE TABLE reviewer_agent_accounts (
 review_id TEXT NOT NULL REFERENCES review(id) ON DELETE CASCADE,
 provider TEXT NOT NULL CHECK(provider IN ('claude','codex')),
 account_id TEXT NOT NULL CHECK(trim(account_id) <> ''),
 resolution_source TEXT NOT NULL CHECK(resolution_source IN ('explicit','role','project','global','device','legacy')),
 bound_at TIMESTAMP NOT NULL,
 PRIMARY KEY(review_id,provider)
);
-- Existing reviewer executions keep their device identities, including the
-- non-active provider used by child tools. Never adopt a new managed default.
INSERT INTO reviewer_agent_accounts(review_id,provider,account_id,resolution_source,bound_at)
 SELECT id,'claude','device-claude','legacy',created_at FROM review;
INSERT INTO reviewer_agent_accounts(review_id,provider,account_id,resolution_source,bound_at)
 SELECT id,'codex','device-codex','legacy',created_at FROM review;

-- +goose StatementBegin
CREATE TRIGGER reviewer_agent_accounts_cdc_insert AFTER INSERT ON reviewer_agent_accounts BEGIN
 INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
 SELECT project_id,session_id,'session_updated',json_object('kind','agent_accounts','reviewId',NEW.review_id),strftime('%Y-%m-%dT%H:%M:%fZ','now') FROM review WHERE id=NEW.review_id;
END;
-- +goose StatementEnd

-- +goose Down
-- Application rollback must retain execution identity once new bindings exist.
CREATE TEMP TABLE account_execution_rollback_guard(unused INTEGER CHECK(unused=1));
INSERT INTO account_execution_rollback_guard(unused)
 SELECT CASE WHEN EXISTS(SELECT 1 FROM reviewer_agent_accounts WHERE resolution_source <> 'legacy')
 OR EXISTS(SELECT 1 FROM session_agent_accounts WHERE resolution_source <> 'legacy')
 OR EXISTS(SELECT 1 FROM automations WHERE agent_account_id <> '')
 OR EXISTS(SELECT 1 FROM projects WHERE COALESCE(json_extract(config,'$.worker.agentAccountId'),'')<>'' OR COALESCE(json_extract(config,'$.orchestrator.agentAccountId'),'')<>'')
 OR EXISTS(SELECT 1 FROM projects AS p, json_each(p.config,'$.agentAccounts') AS a WHERE a.value<>'')
 OR EXISTS(SELECT 1 FROM projects AS p, json_each(p.config,'$.reviewers') AS r WHERE COALESCE(json_extract(r.value,'$.agentAccountId'),'')<>'')
 THEN 0 ELSE 1 END;
DROP TABLE account_execution_rollback_guard;
DROP TRIGGER reviewer_agent_accounts_cdc_insert;
DROP TABLE reviewer_agent_accounts;
ALTER TABLE session_agent_accounts DROP COLUMN resolution_source;
ALTER TABLE automations DROP COLUMN agent_account_id;
