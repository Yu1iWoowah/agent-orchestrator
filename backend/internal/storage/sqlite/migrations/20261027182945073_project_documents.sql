-- +goose Up
CREATE TABLE project_document_states (
 project_id TEXT PRIMARY KEY,
 revision INTEGER NOT NULL CHECK(revision > 0),
 state_json TEXT NOT NULL CHECK(json_valid(state_json))
);

-- +goose Down
DROP TABLE project_document_states;
