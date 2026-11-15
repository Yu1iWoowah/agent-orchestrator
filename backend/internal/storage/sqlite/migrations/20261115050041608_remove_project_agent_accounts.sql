-- +goose Up
-- The project provider account layer was removed. Delete only that key; other
-- settings, role account references, defaults, and execution bindings stay.
-- prepareRemoveProjectAgentAccounts refuses to reach this file while any
-- project config has an abnormal shape, naming each project.
UPDATE projects
SET config = json_remove(config, '$.agentAccounts')
WHERE CASE WHEN json_valid(config) THEN
    CASE WHEN json_type(config) = 'object' THEN json_type(config, '$.agentAccounts') END
END = 'object';

-- +goose Down
-- Removed project provider account references are not restored.
