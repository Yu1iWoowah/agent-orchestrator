import { useInfiniteQuery } from "@tanstack/react-query";
import type { components, paths } from "../../api/schema";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";

export type ProjectAttentionItem =
	components["schemas"]["ProjectAttentionItem"];
type Page = components["schemas"]["ProjectAttentionPage"] & {
	unsupported?: boolean;
};
export type AttentionAudience = "user" | "orchestrator";
export const projectAttentionQueryKey = (projectId: string, hostId?: string) =>
	hostId
		? ["remote-project-attention", hostId, projectId]
		: ["project-attention", projectId];
const record = (value: unknown): value is Record<string, unknown> =>
	typeof value === "object" && value !== null && !Array.isArray(value);
const count = (value: unknown): value is number =>
	typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
function validItem(value: unknown): value is ProjectAttentionItem {
	if (
		!record(value) ||
		typeof value.id !== "string" ||
		typeof value.sessionId !== "string" ||
		typeof value.sessionName !== "string" ||
		typeof value.title !== "string" ||
		!count(value.revision) ||
		typeof value.createdAt !== "string" ||
		!Number.isFinite(Date.parse(value.createdAt)) ||
		typeof value.read !== "boolean"
	)
		return false;
	switch (value.kind) {
		case "agent_question":
			return typeof value.questionId === "string";
		case "worker_report":
			return (
				typeof value.reportId === "string" &&
				typeof value.originHarness === "string" &&
				typeof value.deliveryState === "string" &&
				value.answerState === "unknown"
			);
		case "provider_input":
		case "provider_approval":
			return [
				value.activityId,
				value.conversationId,
				value.branchId,
				value.sourceBranchId,
				value.requestId,
			].every((field) => typeof field === "string");
		default:
			return false;
	}
}
function parsePage(value: unknown): Page {
	if (
		!record(value) ||
		!count(value.open) ||
		!count(value.unread) ||
		value.unread > value.open ||
		!Array.isArray(value.items) ||
		!value.items.every(validItem) ||
		(value.nextCursor !== undefined && typeof value.nextCursor !== "string")
	)
		throw new Error("Invalid project attention response");
	return {
		items: value.items,
		open: value.open,
		unread: value.unread,
		nextCursor: value.nextCursor,
	};
}
export function useProjectAttention(
	projectId: string,
	hostId: string | undefined,
	audience: AttentionAudience,
	enabled = true,
) {
	return useInfiniteQuery({
		queryKey: [...projectAttentionQueryKey(projectId, hostId), audience],
		enabled,
		initialPageParam: "",
		networkMode: "always",
		retry: false,
		queryFn: async ({ pageParam, signal }) => {
			const query = {
				audience,
				cursor: pageParam || undefined,
				limit: 20,
			} satisfies paths["/api/v1/projects/{id}/attention"]["get"]["parameters"]["query"];
			const { data, error, response } = await clientForSessionHost(hostId).GET(
				"/api/v1/projects/{id}/attention",
				{ params: { path: { id: projectId }, query }, signal },
			);
			if (hostId && response.status === 404)
				return { items: [], open: 0, unread: 0, unsupported: true } satisfies Page;
			if (error || !data)
				throw new Error(
					apiErrorMessage(error, "Could not load project attention."),
				);
			return parsePage(data);
		},
		getNextPageParam: (page) => page.nextCursor || undefined,
	});
}
