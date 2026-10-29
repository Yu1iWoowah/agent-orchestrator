-- +goose Up
-- Empty state keeps legacy interrupted targets unknown. New rows explicitly
-- begin at not_started before any external runtime effect can be attempted.
ALTER TABLE session_interface_transitions ADD COLUMN target_runtime_launch_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_interface_transitions ADD COLUMN target_runtime_handle_id TEXT NOT NULL DEFAULT '';
ALTER TABLE session_interface_transitions ADD COLUMN target_runtime_state TEXT NOT NULL DEFAULT '' CHECK(target_runtime_state IN ('','not_started','reserved','started','stopped'));

-- +goose Down
-- Never erase an in-flight writer's ownership evidence during application rollback.
CREATE TEMP TABLE account_runtime_rollback_guard(unused INTEGER CHECK(unused=1));
INSERT INTO account_runtime_rollback_guard(unused)
 SELECT CASE WHEN EXISTS(SELECT 1 FROM session_interface_transitions
  WHERE target_account_id<>'' AND phase NOT IN ('completed','failed','cancelled','recovery_required'))
 THEN 0 ELSE 1 END;
DROP TABLE account_runtime_rollback_guard;
ALTER TABLE session_interface_transitions DROP COLUMN target_runtime_state;
ALTER TABLE session_interface_transitions DROP COLUMN target_runtime_handle_id;
ALTER TABLE session_interface_transitions DROP COLUMN target_runtime_launch_id;
