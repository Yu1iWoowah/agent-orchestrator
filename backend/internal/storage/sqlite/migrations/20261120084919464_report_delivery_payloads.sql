-- +goose Up

CREATE TABLE report_delivery_payloads (
    batch_id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    body TEXT NOT NULL
);

CREATE INDEX idx_report_delivery_payloads_project
    ON report_delivery_payloads(project_id);

-- +goose Down

DROP TABLE report_delivery_payloads;
