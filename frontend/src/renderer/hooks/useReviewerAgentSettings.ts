import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useSyncExternalStore } from "react";
import type { components } from "../../api/schema";
import { getApiBaseUrl, hasTrustedApiBaseUrl, subscribeApiBaseUrl } from "../lib/api-client";
import { isLocal } from "../lib/hosts";
import { clientForSessionHost } from "../lib/host-clients";
import { reviewerAgentSettingsQueryKey } from "../lib/reviewer-agent-settings";
import { appI18n } from "../i18n";

export type ReviewerSettingsIntent = {
	reviewId: string;
	hostId?: string;
	key: string;
	body: Omit<components["schemas"]["ChangeReviewerAgentSettingsPayload"], "expectedRevision"> & { expectedRevision: number };
};

export function useReviewerAgentSettings(reviewId: string, hostId?: string) {
	const baseUrl = useSyncExternalStore(subscribeApiBaseUrl, getApiBaseUrl, getApiBaseUrl);
	const remote = Boolean(hostId && !isLocal(hostId));
	return useQuery({
		queryKey: reviewerAgentSettingsQueryKey(reviewId, hostId),
		enabled: Boolean(reviewId && (remote || baseUrl && hasTrustedApiBaseUrl())),
		queryFn: async ({ signal }) => {
			const { data, error } = await clientForSessionHost(hostId).GET("/api/v1/reviews/{reviewId}/agent-settings", { params: { path: { reviewId } }, signal });
			if (error) throw error;
			if (!data || data.reviewId !== reviewId || !Number.isSafeInteger(data.revision) || data.revision < 0 || data.applyMode !== "next-pass" || data.effectiveState !== "unknown") throw new Error(appI18n.t("reviewerSettings.loadError"));
			return data;
		},
		staleTime: 1_000,
		retry: false,
	});
}

export function usePinReviewerAgentSettings() {
	const client = useQueryClient();
	return useMutation({
		mutationFn: async (intent: ReviewerSettingsIntent) => {
			const { data, error } = await clientForSessionHost(intent.hostId).PATCH("/api/v1/reviews/{reviewId}/agent-settings", {
				params: { path: { reviewId: intent.reviewId } }, headers: { "Idempotency-Key": intent.key }, body: intent.body, signal: AbortSignal.timeout(15_000),
			});
			if (error) throw error;
			if (!data || !data.id || data.reviewId !== intent.reviewId || data.phase !== "pinned" || data.applyMode !== "next-pass"
				|| !Number.isSafeInteger(data.revision) || data.revision !== intent.body.expectedRevision + 1
				|| !data.target || typeof data.target.model !== "string" || typeof data.target.effort !== "string"
				|| (data.target.fastMode !== "default" && data.target.fastMode !== "on" && data.target.fastMode !== "off")) throw new Error(appI18n.t("reviewerSettings.changeError"));
			return data;
		},
		retry: false,
		onSuccess: (receipt, intent) => {
			client.setQueryData<components["schemas"]["ReviewerAgentSettingsResponse"]>(reviewerAgentSettingsQueryKey(intent.reviewId, intent.hostId), (previous) => previous && receipt.revision >= previous.revision
				? { ...previous, revision: receipt.revision, settings: receipt.target }
				: previous);
		},
		onSettled: (_result, _error, intent) => { void client.invalidateQueries({ queryKey: reviewerAgentSettingsQueryKey(intent.reviewId, intent.hostId) }).catch(() => {}); },
	});
}
