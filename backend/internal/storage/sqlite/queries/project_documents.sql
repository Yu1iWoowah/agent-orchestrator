-- name: GetProjectDocumentState :one
SELECT revision, state_json FROM project_document_states WHERE project_id = ?;

-- name: CreateProjectDocumentState :execrows
INSERT INTO project_document_states(project_id, revision, state_json)
VALUES (?, 1, ?) ON CONFLICT(project_id) DO NOTHING;

-- name: UpdateProjectDocumentState :execrows
UPDATE project_document_states SET state_json = ?, revision = revision + 1
WHERE project_id = ? AND revision = ?;
