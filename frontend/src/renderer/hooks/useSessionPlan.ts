import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import type { components, paths } from "../../api/schema";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";

export type SessionPlan = components["schemas"]["SessionPlanResponse"];
export type SessionPlanVersion = components["schemas"]["ConversationPlanVersionResponse"];
export const sessionPlanQueryKey = (sessionId: string, hostId?: string) =>
	hostId
		? (["remote-session-plan", hostId, sessionId] as const)
		: (["session-plan", sessionId] as const);
export class PlanUnavailable extends Error {}
export function useSessionPlan(sessionId: string, hostId?: string) {
	return useQuery({
		queryKey: sessionPlanQueryKey(sessionId, hostId),
		queryFn: async ({ signal }) => {
			const { data, error, response } = await clientForSessionHost(hostId).GET(
				"/api/v1/sessions/{sessionId}/plan",
				{ params: { path: { sessionId } }, signal },
			);
			if (
				response.status === 501 ||
				(response.status === 404 && error?.code !== "SESSION_NOT_FOUND")
			)
				throw new PlanUnavailable("Plan is not supported by this host.");
			if (error || !data) throw new Error(apiErrorMessage(error, "Could not load Plan."));
			return data;
		},
		retry: false,
	});
}
export function useSessionPlanHistory(
	sessionId: string,
	hostId: string | undefined,
	branchId: string | undefined,
	enabled: boolean,
) {
	return useInfiniteQuery({
		queryKey: [...sessionPlanQueryKey(sessionId, hostId), "history", branchId],
		enabled,
		initialPageParam: undefined as number | undefined,
		queryFn: async ({ pageParam, signal }) => {
			const query = {
				beforeSequence: pageParam,
				limit: 20,
			} satisfies paths["/api/v1/sessions/{sessionId}/plan/history"]["get"]["parameters"]["query"];
			const { data, error } = await clientForSessionHost(hostId).GET(
				"/api/v1/sessions/{sessionId}/plan/history",
				{ params: { path: { sessionId }, query }, signal },
			);
			if (error || !data) throw new Error(apiErrorMessage(error, "Could not load Plan history."));
			if (branchId && data.activeBranchId !== branchId)
				throw new Error("The active branch changed. Refresh Plan history.");
			return data;
		},
		getNextPageParam: (page) => (page.hasMoreBefore ? page.nextBeforeSequence : undefined),
		retry: false,
	});
}
