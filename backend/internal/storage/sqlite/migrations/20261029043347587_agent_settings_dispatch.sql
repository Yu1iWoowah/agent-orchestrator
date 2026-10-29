-- +goose Up
ALTER TABLE conversation_turns ADD COLUMN agent_settings_snapshot_json TEXT NOT NULL DEFAULT ''
CHECK (agent_settings_snapshot_json='' OR json_valid(agent_settings_snapshot_json));

-- +goose StatementBegin
CREATE TRIGGER conversation_turn_settings_snapshot_cdc
AFTER UPDATE OF agent_settings_snapshot_json ON conversation_turns
WHEN OLD.agent_settings_snapshot_json <> NEW.agent_settings_snapshot_json
BEGIN
    INSERT INTO change_log(project_id,session_id,event_type,payload,created_at)
    SELECT s.project_id,s.id,'session_updated',
      json_object('id',s.id,'sessionId',s.id,'conversationId',NEW.conversation_id),CURRENT_TIMESTAMP
    FROM conversations c JOIN sessions s ON s.id=c.current_session_id
    WHERE c.id=NEW.conversation_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER conversation_turn_settings_snapshot_cdc;
ALTER TABLE conversation_turns DROP COLUMN agent_settings_snapshot_json;
