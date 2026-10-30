import type { components } from "../../api/schema";
import { apiErrorCode, apiErrorMessage, apiErrorRequestId } from "./api-client";

export type AgentSettingsSnapshot = components["schemas"]["SessionAgentSettingsResponse"];
export type AgentSettingsSelection = components["schemas"]["AgentSettings"];
export type AgentSettingsOperation = components["schemas"]["AgentSettingsOperation"];
export type AgentSettingsPatch = components["schemas"]["AgentSettingsPatch"];

export function agentSettingsErrorMessage(error: unknown, conflictMessage: string): string {
	const code = apiErrorCode(error);
	return [code === "AGENT_SETTINGS_CONFLICT" ? conflictMessage : undefined, apiErrorMessage(error), code, apiErrorRequestId(error)].filter(Boolean).join(" · ");
}

export function agentSettingsQueryKey(sessionId: string, hostId?: string) {
	return hostId
		? ["remote-session-agent-settings", hostId, sessionId] as const
		: ["session-agent-settings", sessionId, null] as const;
}

export function agentSettingsQueryRoot(hostId?: string) {
	return hostId ? ["remote-session-agent-settings", hostId] as const : ["session-agent-settings"] as const;
}

export function canCancelAgentSettings(operation: AgentSettingsOperation): boolean {
	return operation.phase === "pending" && operation.checkpoint === "queued" && !operation.startedAt && !operation.recovery;
}

export function verifiedSettingsActor(operation: AgentSettingsOperation): string | undefined {
	return operation.actor.kind === "orchestrator" && operation.actor.sessionId?.trim()
		? operation.actor.sessionId : undefined;
}

export function settingsDraftPatch(before: AgentSettingsSelection, next: AgentSettingsSelection): AgentSettingsPatch {
	return {
		...(before.model === next.model ? {} : { model: next.model || null }),
		...(before.effort === next.effort ? {} : { effort: next.effort || null }),
		...(before.fastMode === next.fastMode ? {} : { fastMode: next.fastMode }),
	};
}
