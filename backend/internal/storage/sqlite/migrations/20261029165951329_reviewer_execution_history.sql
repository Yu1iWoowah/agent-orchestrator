-- +goose NO TRANSACTION
-- +goose Up
-- Rebuild atomically with FKs temporarily disabled on this migration connection.
-- All old review IDs, child conversations/runs and account bindings survive.
PRAGMA foreign_keys=OFF;
PRAGMA legacy_alter_table=ON;
-- +goose StatementBegin
BEGIN IMMEDIATE;
CREATE TABLE review_next (
    id                 TEXT PRIMARY KEY,
    session_id         TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    project_id         TEXT NOT NULL REFERENCES projects (id),
    harness            TEXT NOT NULL,
    pr_url             TEXT NOT NULL DEFAULT '',
    reviewer_handle_id TEXT NOT NULL DEFAULT '',
    agent_session_id   TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL, reviewer_activity_state TEXT NOT NULL DEFAULT '', reviewer_launch_id TEXT NOT NULL DEFAULT '', interface_mode TEXT NOT NULL DEFAULT 'tui'
    CHECK (interface_mode IN ('tui', 'chat')), provider_conversation_id TEXT NOT NULL DEFAULT '', controller_generation TEXT NOT NULL DEFAULT '', controller_error TEXT NOT NULL DEFAULT '', is_archived BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO review_next SELECT * FROM review;
DROP TABLE review;
ALTER TABLE review_next RENAME TO review;
CREATE UNIQUE INDEX review_current_execution ON review(session_id,harness) WHERE is_archived=0;
CREATE TRIGGER review_conversation_branch_root_provider_update
AFTER UPDATE OF provider_conversation_id ON review
WHEN OLD.provider_conversation_id = '' AND NEW.provider_conversation_id <> ''
BEGIN
    UPDATE conversation_branches
    SET provider_conversation_id = NEW.provider_conversation_id
    WHERE parent_branch_id IS NULL AND provider_conversation_id = ''
      AND id IN (SELECT active_branch_id FROM conversations WHERE current_review_id = NEW.id);
END;
COMMIT;
-- +goose StatementEnd
PRAGMA legacy_alter_table=OFF;
PRAGMA foreign_keys=ON;

-- +goose Down
-- An older schema cannot represent multiple immutable executions. Refuse to
-- discard history; no force cleanup or credential migration is performed.
CREATE TEMP TABLE reviewer_execution_rollback_guard(unused INTEGER CHECK(unused=1));
INSERT INTO reviewer_execution_rollback_guard(unused)
 SELECT CASE WHEN EXISTS(SELECT 1 FROM review GROUP BY session_id,harness HAVING count(*)>1) THEN 0 ELSE 1 END;
DROP TABLE reviewer_execution_rollback_guard;
PRAGMA foreign_keys=OFF;
PRAGMA legacy_alter_table=ON;
-- +goose StatementBegin
BEGIN IMMEDIATE;
CREATE TABLE review_prev (
    id                 TEXT PRIMARY KEY,
    session_id         TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    project_id         TEXT NOT NULL REFERENCES projects (id),
    harness            TEXT NOT NULL,
    pr_url             TEXT NOT NULL DEFAULT '',
    reviewer_handle_id TEXT NOT NULL DEFAULT '',
    agent_session_id   TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL, reviewer_activity_state TEXT NOT NULL DEFAULT '', reviewer_launch_id TEXT NOT NULL DEFAULT '', interface_mode TEXT NOT NULL DEFAULT 'tui'
    CHECK (interface_mode IN ('tui', 'chat')), provider_conversation_id TEXT NOT NULL DEFAULT '', controller_generation TEXT NOT NULL DEFAULT '', controller_error TEXT NOT NULL DEFAULT '', is_archived BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE(session_id, harness)
);
INSERT INTO review_prev SELECT * FROM review;
DROP TABLE review;
ALTER TABLE review_prev RENAME TO review;
CREATE TRIGGER review_conversation_branch_root_provider_update
AFTER UPDATE OF provider_conversation_id ON review
WHEN OLD.provider_conversation_id = '' AND NEW.provider_conversation_id <> ''
BEGIN
    UPDATE conversation_branches
    SET provider_conversation_id = NEW.provider_conversation_id
    WHERE parent_branch_id IS NULL AND provider_conversation_id = ''
      AND id IN (SELECT active_branch_id FROM conversations WHERE current_review_id = NEW.id);
END;
COMMIT;
-- +goose StatementEnd
PRAGMA legacy_alter_table=OFF;
PRAGMA foreign_keys=ON;
