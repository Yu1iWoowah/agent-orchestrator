-- +goose Up
DROP VIEW orchestrator_replacement_released_sessions;
CREATE VIEW orchestrator_replacement_released_sessions AS
SELECT r.id AS operation_id, s.id AS session_id
FROM orchestrator_replacements r
JOIN sessions s ON s.id = json_extract(r.state_json, '$.successorSessionId')
JOIN project_document_states d ON d.project_id = r.project_id
WHERE r.phase = 'notifying' AND r.is_active = 1
 AND json_type(r.state_json, '$.intakeReleasedAt') IS 'text'
 AND json_type(r.state_json, '$.writerActivation') IS 'object'
 AND s.project_id = r.project_id AND s.kind = 'orchestrator'
 AND s.harness = 'claude-code' AND s.session_mode = 'chat' AND s.is_terminated = 0
 AND s.controller_generation = json_extract(d.state_json, '$.lease.owner.ControllerGeneration')
 AND s.provider_conversation_id = json_extract(r.state_json, '$.writerActivation.owner.ProviderConversationID')
 AND (json_extract(d.state_json, '$.lease.claimVerifier') = s.browser_capability_verifier OR (COALESCE(json_extract(d.state_json, '$.lease.claimVerifier'), '') = '' AND COALESCE(json_extract(d.state_json, '$.lease.tokenHash'), '') <> ''))
 AND s.browser_capability_verifier = COALESCE(NULLIF(json_extract(d.state_json, '$.lease.capabilityVerifier'), ''),
   CASE WHEN json_extract(d.state_json, '$.lease.owner') = json_extract(r.state_json, '$.writerActivation.owner')
    THEN json_extract(r.state_json, '$.writerActivation.capabilityVerifier') END)
 AND json_extract(d.state_json, '$.lease.sessionId') = s.id
 AND json_extract(d.state_json, '$.lease.replacementId') = r.id
 AND json_extract(d.state_json, '$.lease.frozen') = 0
 AND json_extract(d.state_json, '$.lease.epoch') = json_extract(r.state_json, '$.writerActivation.afterEpoch')
 AND s.runtime_launch_id = json_extract(d.state_json, '$.lease.owner.RuntimeLaunchID')
 AND s.agent_session_id = json_extract(d.state_json, '$.lease.owner.AgentSessionID')
 AND s.agent_session_id_launch_id = json_extract(d.state_json, '$.lease.owner.AgentSessionIDLaunchID')
 AND json_extract(d.state_json, '$.lease.owner.ProviderConversationID') = s.provider_conversation_id AND json_extract(d.state_json, '$.lease.owner.IsTerminated') = 0 AND json_extract(d.state_json, '$.lease.owner.Harness') = s.harness AND json_extract(d.state_json, '$.lease.owner.Mode') = s.session_mode
UNION
-- A paused pre-stop attempt may return ordinary input to its original owner.
-- A later owner/lease rotation closes this release without trusting the marker.
SELECT r.id AS operation_id, s.id AS session_id
FROM orchestrator_replacements r
JOIN sessions s ON s.id = r.source_session_id
LEFT JOIN project_document_states d ON d.project_id = r.project_id
WHERE r.is_active = 1 AND r.phase IN ('awaiting_source', 'awaiting_handoff', 'awaiting_transcript')
 AND json_type(r.state_json, '$.sourceIntakeReleasedAt') IS 'text'
 AND COALESCE(json_extract(r.state_json, '$.successorSessionId'), '') = ''
 AND json_type(r.state_json, '$.sourceStopIntentAt') IS NULL
 AND json_type(r.state_json, '$.sourceStop') IS NULL
 AND json_type(r.state_json, '$.sourceRetiredAt') IS NULL
 AND s.project_id = r.project_id AND s.kind = 'orchestrator'
 AND s.is_terminated = 0 AND s.hibernated_at IS NULL AND s.activity_state <> 'exited'
 AND s.harness = json_extract(r.state_json, '$.sourceOwner.Harness')
 AND s.session_mode = json_extract(r.state_json, '$.sourceOwner.Mode')
 AND s.controller_generation = json_extract(r.state_json, '$.sourceOwner.ControllerGeneration')
 AND s.provider_conversation_id = json_extract(r.state_json, '$.sourceOwner.ProviderConversationID')
 AND s.runtime_launch_id = json_extract(r.state_json, '$.sourceOwner.RuntimeLaunchID')
 AND s.agent_session_id = json_extract(r.state_json, '$.sourceOwner.AgentSessionID')
 AND s.agent_session_id_launch_id = json_extract(r.state_json, '$.sourceOwner.AgentSessionIDLaunchID')
 AND s.browser_capability_verifier = json_extract(r.state_json, '$.sourceCapabilityVerifier')
 AND (d.project_id IS NULL OR json_type(d.state_json, '$.lease') IS NOT 'object'
  OR (json_extract(d.state_json, '$.lease.sessionId') = s.id
   AND json_extract(d.state_json, '$.lease.owner') = json_extract(r.state_json, '$.sourceOwner')
   AND json_extract(d.state_json, '$.lease.frozen') = 0
   AND COALESCE(json_extract(d.state_json, '$.lease.replacementId'), '') IN ('', r.id)
   AND (COALESCE(json_extract(r.state_json, '$.preparation.leaseEpoch'), 0) = 0
    OR json_extract(d.state_json, '$.lease.epoch') = json_extract(r.state_json, '$.preparation.leaseEpoch'))));

-- +goose Down
DROP VIEW orchestrator_replacement_released_sessions;
CREATE VIEW orchestrator_replacement_released_sessions AS
SELECT r.id AS operation_id, s.id AS session_id
FROM orchestrator_replacements r
JOIN sessions s ON s.id = json_extract(r.state_json, '$.successorSessionId')
JOIN project_document_states d ON d.project_id = r.project_id
WHERE r.phase = 'notifying' AND r.is_active = 1
 AND json_type(r.state_json, '$.intakeReleasedAt') IS 'text'
 AND json_type(r.state_json, '$.writerActivation') IS 'object'
 AND s.project_id = r.project_id AND s.kind = 'orchestrator'
 AND s.harness = 'claude-code' AND s.session_mode = 'chat' AND s.is_terminated = 0
 AND s.controller_generation = json_extract(r.state_json, '$.writerActivation.owner.ControllerGeneration')
 AND s.provider_conversation_id = json_extract(r.state_json, '$.writerActivation.owner.ProviderConversationID')
 AND s.browser_capability_verifier = json_extract(r.state_json, '$.writerActivation.capabilityVerifier')
 AND json_extract(d.state_json, '$.lease.sessionId') = s.id
 AND json_extract(d.state_json, '$.lease.replacementId') = r.id
 AND json_extract(d.state_json, '$.lease.frozen') = 0
 AND json_extract(d.state_json, '$.lease.epoch') = json_extract(r.state_json, '$.writerActivation.afterEpoch')
 AND json_extract(d.state_json, '$.lease.owner') = json_extract(r.state_json, '$.writerActivation.owner')
UNION
-- A paused pre-stop attempt may return ordinary input to its original owner.
-- A later owner/lease rotation closes this release without trusting the marker.
SELECT r.id AS operation_id, s.id AS session_id
FROM orchestrator_replacements r
JOIN sessions s ON s.id = r.source_session_id
LEFT JOIN project_document_states d ON d.project_id = r.project_id
WHERE r.is_active = 1 AND r.phase IN ('awaiting_source', 'awaiting_handoff', 'awaiting_transcript')
 AND json_type(r.state_json, '$.sourceIntakeReleasedAt') IS 'text'
 AND COALESCE(json_extract(r.state_json, '$.successorSessionId'), '') = ''
 AND json_type(r.state_json, '$.sourceStopIntentAt') IS NULL
 AND json_type(r.state_json, '$.sourceStop') IS NULL
 AND json_type(r.state_json, '$.sourceRetiredAt') IS NULL
 AND s.project_id = r.project_id AND s.kind = 'orchestrator'
 AND s.is_terminated = 0 AND s.hibernated_at IS NULL AND s.activity_state <> 'exited'
 AND s.harness = json_extract(r.state_json, '$.sourceOwner.Harness')
 AND s.session_mode = json_extract(r.state_json, '$.sourceOwner.Mode')
 AND s.controller_generation = json_extract(r.state_json, '$.sourceOwner.ControllerGeneration')
 AND s.provider_conversation_id = json_extract(r.state_json, '$.sourceOwner.ProviderConversationID')
 AND s.runtime_launch_id = json_extract(r.state_json, '$.sourceOwner.RuntimeLaunchID')
 AND s.agent_session_id = json_extract(r.state_json, '$.sourceOwner.AgentSessionID')
 AND s.agent_session_id_launch_id = json_extract(r.state_json, '$.sourceOwner.AgentSessionIDLaunchID')
 AND s.browser_capability_verifier = json_extract(r.state_json, '$.sourceCapabilityVerifier')
 AND (d.project_id IS NULL OR json_type(d.state_json, '$.lease') IS NOT 'object'
  OR (json_extract(d.state_json, '$.lease.sessionId') = s.id
   AND json_extract(d.state_json, '$.lease.owner') = json_extract(r.state_json, '$.sourceOwner')
   AND json_extract(d.state_json, '$.lease.frozen') = 0
   AND COALESCE(json_extract(d.state_json, '$.lease.replacementId'), '') IN ('', r.id)
   AND (COALESCE(json_extract(r.state_json, '$.preparation.leaseEpoch'), 0) = 0
    OR json_extract(d.state_json, '$.lease.epoch') = json_extract(r.state_json, '$.preparation.leaseEpoch'))));
