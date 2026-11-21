-- +goose Up
CREATE TABLE reviewer_stop_receipts (
    review_id TEXT NOT NULL CHECK (review_id <> ''),
    launch_id TEXT NOT NULL,
    session_id TEXT NOT NULL CHECK (session_id <> ''),
    project_id TEXT,
    harness TEXT NOT NULL CHECK (harness <> ''),
    handle_id TEXT NOT NULL,
    interface_mode TEXT NOT NULL CHECK (interface_mode IN ('chat', 'tui')),
    controller_generation TEXT NOT NULL DEFAULT '',
    data_root TEXT NOT NULL DEFAULT '',
    boot_id TEXT NOT NULL DEFAULT '',
    pid_namespace TEXT NOT NULL DEFAULT '',
    chat_scope TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('capturing', 'sealed', 'unknown')),
    reason TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (review_id, launch_id),
    CHECK (state = 'unknown' OR (launch_id <> '' AND data_root <> '' AND boot_id <> '' AND pid_namespace <> ''))
);
CREATE INDEX idx_reviewer_stop_receipts_session ON reviewer_stop_receipts(session_id);

CREATE TABLE reviewer_stop_accounts (
    review_id TEXT NOT NULL,
    launch_id TEXT NOT NULL,
    provider TEXT NOT NULL CHECK (provider IN ('claude', 'codex')),
    account_id TEXT NOT NULL CHECK (trim(account_id) <> ''),
    PRIMARY KEY (review_id, launch_id, provider),
    FOREIGN KEY (review_id, launch_id) REFERENCES reviewer_stop_receipts(review_id, launch_id) ON DELETE CASCADE
);
CREATE INDEX idx_reviewer_stop_accounts_owner ON reviewer_stop_accounts(account_id, review_id, launch_id);

CREATE TABLE reviewer_stop_processes (
    review_id TEXT NOT NULL,
    launch_id TEXT NOT NULL,
    pid INTEGER NOT NULL CHECK (pid > 1),
    start_identity TEXT NOT NULL CHECK (start_identity <> ''),
    started_at TIMESTAMP,
    PRIMARY KEY (review_id, launch_id, pid, start_identity),
    FOREIGN KEY (review_id, launch_id) REFERENCES reviewer_stop_receipts(review_id, launch_id) ON DELETE CASCADE
);

INSERT INTO reviewer_stop_receipts (review_id, launch_id, session_id, project_id, harness, handle_id, interface_mode, controller_generation, state, reason)
SELECT r.id, r.reviewer_launch_id, r.session_id, r.project_id, r.harness, r.reviewer_handle_id, r.interface_mode, r.controller_generation, 'unknown', 'historical_missing_lineage'
FROM review AS r WHERE r.is_archived=0 AND r.reviewer_launch_id<>'' AND r.reviewer_handle_id<>''
AND EXISTS (SELECT 1 FROM reviewer_agent_accounts AS a WHERE a.review_id=r.id)
AND EXISTS (SELECT 1 FROM review_run AS p WHERE p.review_id=r.id AND p.launch_id=r.reviewer_launch_id
            AND p.status='failed' AND p.failure_reason IN ('cancel_unconfirmed', 'never_started', 'timed_out'));

INSERT INTO reviewer_stop_accounts (review_id, launch_id, provider, account_id)
SELECT s.review_id, s.launch_id, a.provider, a.account_id
FROM reviewer_stop_receipts AS s JOIN reviewer_agent_accounts AS a ON a.review_id=s.review_id;

-- +goose Down
CREATE TEMP TABLE reviewer_stop_rollback_guard (empty INTEGER CHECK (empty=1));
INSERT INTO reviewer_stop_rollback_guard SELECT NOT EXISTS (SELECT 1 FROM reviewer_stop_receipts);
DROP TABLE reviewer_stop_rollback_guard;
DROP TABLE reviewer_stop_processes;
DROP TABLE reviewer_stop_accounts;
DROP TABLE reviewer_stop_receipts;
