import { useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { useSyncExternalStore } from "react";
import { getApiBaseUrl, hasTrustedApiBaseUrl, subscribeApiBaseUrl } from "../lib/api-client";
import { agentSettingsQueryKey } from "../lib/agent-settings";
import { clientForSessionHost } from "../lib/host-clients";
import { appI18n } from "../i18n";
import { isLocal } from "../lib/hosts";
import { conversationConfigOptionsQueryKey, conversationModelsQueryKey, conversationQueryKey } from "./useConversation";

export function useSessionAgentSettings({ sessionId, hostId, enabled = true }: {
	sessionId?: string;
	hostId?: string;
	enabled?: boolean;
}) {
	const baseUrl = useSyncExternalStore(subscribeApiBaseUrl, getApiBaseUrl, getApiBaseUrl);
	const remote = Boolean(hostId && !isLocal(hostId));
	return useQuery({
		queryKey: agentSettingsQueryKey(sessionId ?? "", hostId),
		enabled: enabled && Boolean(sessionId && (remote || (baseUrl && hasTrustedApiBaseUrl()))),
		queryFn: async () => {
			if (!sessionId) throw new Error("A session is required");
			const { data, error } = await clientForSessionHost(hostId).GET("/api/v1/sessions/{sessionId}/agent-settings", {
				params: { path: { sessionId } },
			});
			if (error) throw error;
			if (!data || data.sessionId !== sessionId || !Number.isSafeInteger(data.revision) || data.revision < 0
				|| !data.settings || !data.capabilities || !data.effective?.model || !data.effective.effort || !data.effective.fastMode
				|| (data.capabilities.models != null && !Array.isArray(data.capabilities.models))) {
				throw new Error(appI18n.t("agentSettings.loadError"));
			}
			// Unknown capability responses can encode an empty Go slice as null.
			// This is no evidence of support and offers no model choices.
			return { ...data, capabilities: { ...data.capabilities, models: data.capabilities.models ?? [] } };
		},
		staleTime: 1_000,
		refetchInterval: (query) => query.state.data?.pending ? 1_000 : false,
		retry: false,
	});
}

export type AgentSettingsChangeIntent = {
	sessionId: string;
	hostId?: string;
	key: string;
	body: components["schemas"]["ChangeSessionAgentSettingsPayload"];
};

async function refreshSettingsViews(client: QueryClient, target: { sessionId: string; hostId?: string }) {
	await client.invalidateQueries({ queryKey: agentSettingsQueryKey(target.sessionId, target.hostId) });
	// The snapshot may already be applied before its CDC arrives. A write or
	// cancellation invalidates every projection even if CDC sees a stable stamp.
	const hostId = target.hostId && !isLocal(target.hostId) ? target.hostId : undefined;
	await Promise.all([
		client.invalidateQueries({ queryKey: conversationQueryKey(target.sessionId, hostId) }),
		client.invalidateQueries({ queryKey: conversationModelsQueryKey(target.sessionId, hostId) }),
		client.invalidateQueries({ queryKey: conversationConfigOptionsQueryKey(target.sessionId, hostId) }),
	]);
}

/** Retry is an explicit user action, with the original key and captured CAS. */
export function useChangeAgentSettings() {
	const client = useQueryClient();
	return useMutation({
		mutationFn: async (intent: AgentSettingsChangeIntent) => {
			const { data, error } = await clientForSessionHost(intent.hostId).PATCH("/api/v1/sessions/{sessionId}/agent-settings", {
				params: { path: { sessionId: intent.sessionId } },
				headers: { "Idempotency-Key": intent.key },
				body: intent.body,
				signal: AbortSignal.timeout(15_000),
			});
			if (error) throw error;
			if (!data) throw new Error(appI18n.t("agentSettings.changeError"));
			return data.operation;
		},
		retry: false,
		onSettled: (_data, _error, intent) => refreshSettingsViews(client, intent),
	});
}

type AgentSettingsAction = { sessionId: string; hostId?: string } & (
	| { kind: "cancel"; operationId: string; version: number }
	| { kind: "settle"; operationId: string; version: number; stopIfAlive: boolean }
);

export function useAgentSettingsAction() {
	const client = useQueryClient();
	return useMutation({
		mutationFn: async (action: AgentSettingsAction) => {
			const api = clientForSessionHost(action.hostId);
			const result = action.kind === "cancel"
					? await api.POST("/api/v1/sessions/{sessionId}/agent-settings/operations/{operationId}/cancel", {
						params: { path: { sessionId: action.sessionId, operationId: action.operationId } }, body: { expectedVersion: action.version }, signal: AbortSignal.timeout(15_000),
					})
					: await api.POST("/api/v1/sessions/{sessionId}/agent-settings/operations/{operationId}/settle", {
						params: { path: { sessionId: action.sessionId, operationId: action.operationId } }, body: { expectedVersion: action.version, stopIfAlive: action.stopIfAlive }, signal: AbortSignal.timeout(30_000),
					});
			if (result.error) throw result.error;
			if (!result.data) throw new Error(appI18n.t("agentSettings.changeError"));
			return result.data;
		},
		retry: false,
		onSettled: (_data, _error, action) => refreshSettingsViews(client, action),
	});
}
