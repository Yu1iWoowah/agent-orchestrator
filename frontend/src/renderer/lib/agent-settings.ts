import type { components } from "../../api/schema";
import { apiErrorCode, apiErrorMessage, apiErrorRequestId } from "./api-client";
import { isLocal } from "./hosts";

export type AgentSettingsSnapshot = components["schemas"]["SessionAgentSettingsResponse"];
export type AgentSettingsSelection = components["schemas"]["AgentSettings"];
export type AgentSettingsOperation = components["schemas"]["AgentSettingsOperation"];
export type AgentSettingsPatch = components["schemas"]["AgentSettingsPatch"];

/** History is newest first. A newer result supersedes an older failure. */
export function latestAgentSettingsFailure(snapshot: AgentSettingsSnapshot): AgentSettingsOperation | undefined {
	const latest = snapshot.history?.[0];
	return !snapshot.pending && latest?.phase === "failed" && latest.sessionId === snapshot.sessionId ? latest : undefined;
}

export function agentSettingsErrorMessage(error: unknown, conflictMessage: string): string {
	const code = apiErrorCode(error);
	return [code === "AGENT_SETTINGS_CONFLICT" ? conflictMessage : undefined, apiErrorMessage(error), code, apiErrorRequestId(error)].filter(Boolean).join(" · ");
}

export function agentSettingsQueryKey(sessionId: string, hostId?: string) {
	return hostId && !isLocal(hostId)
		? ["remote-session-agent-settings", hostId, sessionId] as const
		: ["session-agent-settings", sessionId, null] as const;
}

export function agentSettingsQueryRoot(hostId?: string) {
	return hostId && !isLocal(hostId) ? ["remote-session-agent-settings", hostId] as const : ["session-agent-settings"] as const;
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

export function rebaseAgentSettingsSelection(before: AgentSettingsSelection, selected: AgentSettingsSelection, current: AgentSettingsSelection): AgentSettingsSelection {
	const changes = settingsDraftPatch(before, selected);
	return {
		model: changes.model === undefined ? current.model : changes.model ?? "",
		effort: changes.effort === undefined ? current.effort : changes.effort ?? "",
		fastMode: changes.fastMode ?? current.fastMode,
	};
}
