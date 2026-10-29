import {
	useInfiniteQuery,
	useMutation,
	useQueries,
	useQuery,
	useQueryClient,
	queryOptions,
} from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { components, paths } from "../../api/schema";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import { toActivity } from "./useConversation";
import { useProviderInputDraftStore } from "../stores/provider-input-draft-store";

export type AttentionItem = components["schemas"]["AttentionItemResponse"];
type AttentionPage = components["schemas"]["SessionAttentionResponse"];

function checkedAttentionResponse(data: AttentionPage): AttentionPage {
	if (
		!Array.isArray(data.items) ||
		!data.counts ||
		![data.counts.open, data.counts.unread, data.counts.history].every(
			Number.isFinite,
		) ||
		typeof data.activeBranchId !== "string" ||
		typeof data.hasMoreBefore !== "boolean" ||
		!data.items.every((item) => typeof item?.activity?.id === "string")
	)
		throw new Error("Invalid attention response.");
	return data;
}
export const sessionAttentionQueryKey = (sessionId: string, hostId?: string) =>
	hostId
		? (["remote-session-attention", hostId, sessionId] as const)
		: (["session-attention", sessionId] as const);
export class AttentionUnavailable extends Error {}
export class AttentionSourceUnavailable extends Error {}

async function fetchAttentionSource(
	sessionId: string,
	hostId: string | undefined,
	activityId: string,
	signal: AbortSignal,
) {
	const { data, error } = await clientForSessionHost(hostId).GET(
		"/api/v1/sessions/{sessionId}/attention/{activityId}",
		{ params: { path: { sessionId, activityId } }, signal },
	);
	if (error?.code === "ATTENTION_CHANGED")
		throw new AttentionSourceUnavailable();
	if (error || !data)
		throw new Error(
			apiErrorMessage(error, "The request is no longer available."),
		);
	return checkedAttentionResponse(data);
}


export function attentionSourceQueryOptions({
	sessionId,
	hostId,
	branchId,
	activityId,
}: {
	sessionId: string;
	hostId?: string;
	branchId?: string;
	activityId: string;
}) {
	return queryOptions({
		queryKey: [
			...sessionAttentionQueryKey(sessionId, hostId),
			"source",
			branchId,
			activityId,
		],
		queryFn: ({ signal }) =>
			fetchAttentionSource(sessionId, hostId, activityId, signal),
		// Explicit opens fetch with staleTime: 0; CDC refreshes this shared entry.
		staleTime: Infinity,
		retry: false,
	});
}

/** Keep drafted sources observed after selection moves to another request. */
function useAttentionDraftCleanup(
	sessionId: string,
	hostId: string | undefined,
	branchId: string | undefined,
	enabled: boolean,
) {
	const drafts = useProviderInputDraftStore((state) => state.drafts);
	const sources = useMemo(() => {
		const scoped = new Map<
			string,
			{ conversationId: string; activityId: string; revision: number }
		>();
		if (!enabled || branchId === undefined) return [];
		for (const key of Object.keys(drafts)) {
			let parts: unknown;
			try {
				parts = JSON.parse(key);
			} catch {
				continue;
			}
			if (
				!Array.isArray(parts) ||
				parts.length !== 6 ||
				parts[0] !== (hostId ?? null) ||
				parts[1] !== sessionId ||
				parts[3] !== branchId ||
				typeof parts[2] !== "string" ||
				typeof parts[4] !== "string" ||
				typeof parts[5] !== "number" ||
				!Number.isFinite(parts[5])
			)
				continue;
			const source = {
				conversationId: parts[2],
				activityId: parts[4],
				revision: parts[5],
			};
			const id = JSON.stringify([source.conversationId, source.activityId]);
			const previous = scoped.get(id);
			if (!previous || previous.revision < source.revision)
				scoped.set(id, source);
		}
		return [...scoped.values()];
	}, [drafts, enabled, hostId, sessionId, branchId]);
	const queries = useQueries({
		queries: sources.map((source) =>
			attentionSourceQueryOptions({
				sessionId,
				hostId,
				branchId,
				activityId: source.activityId,
			}),
		),
	});
	useEffect(() => {
		for (const [index, source] of sources.entries()) {
			const query = queries[index];
			const data = query?.data;
			if (
				!query?.isSuccess ||
				!data ||
				data.conversationId !== source.conversationId ||
				data.activeBranchId !== branchId
			)
				continue;
			if (
				data.items.some(
					(item) =>
						item.branchId === branchId &&
						item.activity.id === source.activityId &&
						!item.actionable &&
						item.activity.revision >= source.revision &&
						["resolved", "completed", "cancelled"].includes(
							item.activity.status,
						),
				)
			) {
				useProviderInputDraftStore.getState().clearSource({
					hostId,
					sessionId,
					branchId,
					activityId: source.activityId,
				});
			}
		}
	}, [queries, sources, hostId, sessionId, branchId]);
}

export function useSessionAttention(
	sessionId: string,
	hostId: string | undefined,
	branchId: string | undefined,
	state: "open" | "history",
	enabled = true,
) {
	useAttentionDraftCleanup(
		sessionId,
		hostId,
		branchId,
		enabled && state === "open",
	);
	return useInfiniteQuery({
		queryKey: [...sessionAttentionQueryKey(sessionId, hostId), branchId, state],
		enabled,
		initialPageParam: undefined as number | undefined,
		queryFn: async ({ pageParam, signal }) => {
			const query = {
				state,
				beforeSequence: pageParam,
				limit: 20,
			} satisfies paths["/api/v1/sessions/{sessionId}/attention"]["get"]["parameters"]["query"];
			const { data, error, response } = await clientForSessionHost(hostId).GET(
				"/api/v1/sessions/{sessionId}/attention",
				{ params: { path: { sessionId }, query }, signal },
			);
			if (
				response.status === 501 ||
				(response.status === 404 && error?.code !== "SESSION_NOT_FOUND")
			)
				throw new AttentionUnavailable();
			if (error || !data)
				throw new Error(apiErrorMessage(error, "Could not load attention."));
			if (branchId && data.activeBranchId !== branchId)
				throw new Error("The active branch changed.");
			return checkedAttentionResponse(data);
		},
		getNextPageParam: (page) =>
			page.hasMoreBefore ? page.nextBeforeSequence : undefined,
		retry: false,
	});
}

/** A fresh lookup selects the one original composer form, never a duplicate. */
export function useAttentionSelection(
	sessionId: string,
	hostId: string | undefined,
	branchId: string | undefined,
	enabled: boolean,
) {
	const client = useQueryClient();
	const scope = JSON.stringify([hostId, sessionId, branchId]);
	const nonce = useRef(0);
	const [selected, setSelected] = useState<{
		scope: string;
		id: string;
		nonce: number;
		lookup: "loading" | "ready" | "failed";
	}>();
	const active = enabled && selected?.scope === scope;
	const source = useQuery({
		...attentionSourceQueryOptions({
			sessionId,
			hostId,
			branchId,
			activityId: active ? selected.id : "",
		}),
		enabled: active,
	});
	const markRead = useMutation({
		mutationFn: async (input: {
			sessionId: string;
			hostId?: string;
			activityId: string;
			branchId: string;
			revision: number;
		}) => {
			const { error } = await clientForSessionHost(input.hostId).POST(
				"/api/v1/sessions/{sessionId}/attention/{activityId}/read",
				{
					params: {
						path: { sessionId: input.sessionId, activityId: input.activityId },
					},
					body: {
						expectedBranchId: input.branchId,
						expectedRevision: input.revision,
					},
				},
			);
			if (error)
				throw new Error(
					apiErrorMessage(error, "Could not mark the request read."),
				);
		},
		onSuccess: (_data, input) =>
			client.invalidateQueries({
				queryKey: sessionAttentionQueryKey(input.sessionId, input.hostId),
			}, { cancelRefetch: false }),
	});
	const data = active && selected.lookup === "ready" && !source.isError
		? source.data
		: undefined;
	const candidate =
		data && data.activeBranchId === branchId ? data.items[0] : undefined;
	const current =
		candidate?.actionable &&
		(candidate.activity.activityKind === "approval" ||
			candidate.activity.activityKind === "user_input")
			? candidate
			: undefined;
	const missing =
		(active && selected.lookup !== "loading" && source.error instanceof AttentionSourceUnavailable) ||
		Boolean(
			data && data.activeBranchId === branchId && data.items.length === 0,
		);
	const unavailable =
		active && selected.lookup !== "loading" && (missing || Boolean(candidate && !candidate.actionable));
	const clear = useCallback(
		({ discardDraft = true }: { discardDraft?: boolean } = {}) => {
			if (!active || !selected) return;
			if (discardDraft)
				useProviderInputDraftStore
					.getState()
					.clearSource({
						hostId,
						sessionId,
						branchId,
						activityId: selected.id,
					});
			// A late answer for A must not release a newer B (or a newer locate of A).
			setSelected((value) => (value === selected ? undefined : value));
		},
		[active, selected, hostId, sessionId, branchId],
	);
	useEffect(() => {
		if (unavailable)
			clear({
				discardDraft:
					missing ||
					Boolean(
						candidate &&
						["resolved", "completed", "cancelled"].includes(
							candidate.activity.status,
						),
					),
			});
	}, [unavailable, missing, candidate, clear]);
	const activity = useMemo(
		() => (current ? toActivity(current.activity) : undefined),
		[current],
	);
	const locate = useCallback(
		(id: string) => {
			markRead.reset();
			const requestNonce = ++nonce.current;
			setSelected({ scope, id, nonce: requestNonce, lookup: "loading" });
			const finished = (lookup: "ready" | "failed") =>
				setSelected((value) =>
					value?.scope === scope && value.nonce === requestNonce
						? { ...value, lookup }
						: value,
				);
			void client.fetchQuery({
				...attentionSourceQueryOptions({ sessionId, hostId, branchId, activityId: id }),
				staleTime: 0,
			}).then(() => finished("ready"), () => finished("failed"));
		},
		[scope, markRead.reset, client, sessionId, hostId, branchId],
	);
	return {
		scope,
		locate,
		clear,
		selectedId: active ? selected.id : undefined,
		selectionNonce: active ? selected.nonce : undefined,
		activity,
		loading: active && (selected.lookup === "loading" || source.isPending),
		error:
			active && selected.lookup !== "loading" &&
			(selected.lookup === "failed" || source.isError || (source.data && !current && !unavailable))
				? true
				: false,
		unavailable,
		readError: active && markRead.isError,
		markViewed: () =>
			current && !current.read && source.data
				? markRead.mutate({
						sessionId,
						hostId,
						activityId: current.activity.id,
						branchId: source.data.activeBranchId,
						revision: current.activity.revision,
					})
				: undefined,
	};
}
