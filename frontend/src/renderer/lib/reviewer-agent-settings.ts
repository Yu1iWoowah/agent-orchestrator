import { isLocal } from "./hosts";

export function reviewerAgentSettingsQueryKey(reviewId: string, hostId?: string) {
	return hostId && !isLocal(hostId) ? ["reviewer-agent-settings", hostId, reviewId] as const : ["reviewer-agent-settings", null, reviewId] as const;
}
