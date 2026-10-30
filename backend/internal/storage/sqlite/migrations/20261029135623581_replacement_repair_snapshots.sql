-- +goose Up
CREATE TABLE orchestrator_replacement_repair_snapshots (
 operation_id TEXT NOT NULL REFERENCES orchestrator_replacements(id),
 revision INTEGER NOT NULL CHECK(revision > 0),
 digest TEXT NOT NULL CHECK(length(digest) = 64),
 payload BLOB NOT NULL,
 PRIMARY KEY(operation_id, revision)
);

-- Only managed store commits insert checkpoints. Never capture arbitrary row
-- updates through a trigger: a corrupt row must not authenticate itself.
-- +goose StatementBegin
CREATE TRIGGER replacement_repair_snapshot_no_update
BEFORE UPDATE ON orchestrator_replacement_repair_snapshots
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPAIR_SNAPSHOT_IMMUTABLE');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER replacement_repair_snapshot_no_delete
BEFORE DELETE ON orchestrator_replacement_repair_snapshots
BEGIN
 SELECT RAISE(ABORT, 'ORCHESTRATOR_REPAIR_SNAPSHOT_IMMUTABLE');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER replacement_repair_snapshot_no_delete;
DROP TRIGGER replacement_repair_snapshot_no_update;
DROP TABLE orchestrator_replacement_repair_snapshots;
