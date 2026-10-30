-- Same-harness settings restarts share the physical controller handoff gate.
-- Follow 0211's surgical CHECK rewrite; retain unfinished settings recovery.
-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
ALTER TABLE session_interface_transitions ADD COLUMN settings_operation_id TEXT NOT NULL DEFAULT '' CHECK (settings_operation_id = '' OR settings_operation_id = id);
-- +goose StatementEnd
-- +goose StatementBegin
PRAGMA writable_schema = ON;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE sqlite_master SET sql = replace(sql,
 'CHECK (source_mode <> target_mode OR target_account_id <> '''')',
 'CHECK (source_mode <> target_mode OR target_account_id <> '''' OR (source_mode = ''tui'' AND settings_operation_id <> ''''))')
WHERE type='table' AND name='session_interface_transitions';
-- +goose StatementEnd
-- +goose StatementBegin
PRAGMA writable_schema = RESET;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_session_interface_transition_active;
CREATE UNIQUE INDEX idx_session_interface_transition_active ON session_interface_transitions(session_id)
WHERE phase NOT IN ('completed','failed','cancelled','recovery_required') OR (phase='recovery_required' AND settings_operation_id <> '');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER tui_settings_handoff_owner_insert BEFORE INSERT ON session_interface_transitions
WHEN NEW.settings_operation_id<>''
BEGIN
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM session_agent_settings_operations WHERE id=NEW.settings_operation_id AND session_id=NEW.session_id AND apply_mode='restart') THEN RAISE(ABORT,'settings handoff owner missing') END;
END;
CREATE TRIGGER tui_settings_handoff_owner_update BEFORE UPDATE OF settings_operation_id,id,session_id ON session_interface_transitions
WHEN NEW.settings_operation_id<>''
BEGIN
 SELECT CASE WHEN NOT EXISTS(SELECT 1 FROM session_agent_settings_operations WHERE id=NEW.settings_operation_id AND session_id=NEW.session_id AND apply_mode='restart') THEN RAISE(ABORT,'settings handoff owner missing') END;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Refuse downgrade rather than discard physical-writer history or fences.
CREATE TEMP TABLE tui_settings_downgrade_guard (ok INTEGER CHECK (ok=1));
INSERT INTO tui_settings_downgrade_guard SELECT 0 FROM session_interface_transitions WHERE settings_operation_id<>'' LIMIT 1;
DROP TABLE tui_settings_downgrade_guard;
DROP TRIGGER tui_settings_handoff_owner_insert;
DROP TRIGGER tui_settings_handoff_owner_update;
DROP INDEX idx_session_interface_transition_active;
CREATE UNIQUE INDEX idx_session_interface_transition_active ON session_interface_transitions(session_id) WHERE phase NOT IN ('completed','failed','cancelled','recovery_required');
-- +goose StatementEnd
-- +goose StatementBegin
PRAGMA writable_schema = ON;
-- +goose StatementEnd
-- +goose StatementBegin
UPDATE sqlite_master SET sql = replace(sql,
 'CHECK (source_mode <> target_mode OR target_account_id <> '''' OR (source_mode = ''tui'' AND settings_operation_id <> ''''))',
 'CHECK (source_mode <> target_mode OR target_account_id <> '''')')
WHERE type='table' AND name='session_interface_transitions';
-- +goose StatementEnd
-- +goose StatementBegin
PRAGMA writable_schema = RESET;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE session_interface_transitions DROP COLUMN settings_operation_id;
-- +goose StatementEnd
