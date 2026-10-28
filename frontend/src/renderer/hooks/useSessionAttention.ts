import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useCallback, useMemo, useRef, useState } from "react";
import type { components, paths } from "../../api/schema";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import { toActivity } from "./useConversation";

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

export function useSessionAttention(
  sessionId: string,
  hostId: string | undefined,
  branchId: string | undefined,
  state: "open" | "history",
  enabled = true,
) {
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
    refetchInterval: hostId ? 2000 : false,
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
  }>();
  const active = enabled && selected?.scope === scope;
  const source = useQuery({
    queryKey: [
      ...sessionAttentionQueryKey(sessionId, hostId),
      "source",
      branchId,
      active ? selected.id : undefined,
      active ? selected.nonce : undefined,
    ],
    enabled: active,
    queryFn: async ({ signal }) => {
      const { data, error } = await clientForSessionHost(hostId).GET(
        "/api/v1/sessions/{sessionId}/attention/{activityId}",
        {
          params: { path: { sessionId, activityId: selected?.id ?? "" } },
          signal,
        },
      );
      if (error || !data)
        throw new Error(
          apiErrorMessage(error, "The request is no longer available."),
        );
      return checkedAttentionResponse(data);
    },
    retry: false,
    refetchInterval: hostId && active ? 2000 : false,
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
      }),
  });
  const data = active ? source.data : undefined;
  const candidate =
    data && data.activeBranchId === branchId ? data.items[0] : undefined;
  const current =
    candidate?.actionable &&
    (candidate.activity.activityKind === "approval" ||
      candidate.activity.activityKind === "user_input")
      ? candidate
      : undefined;
  const unavailable = Boolean(candidate && !candidate.actionable);
  const activity = useMemo(
    () => (current ? toActivity(current.activity) : undefined),
    [current],
  );
  const locate = useCallback(
    (id: string) => {
      markRead.reset();
      setSelected({ scope, id, nonce: ++nonce.current });
    },
    [scope, markRead.reset],
  );
  return {
    scope,
    locate,
    selectedId: active ? selected.id : undefined,
    selectionNonce: active ? selected.nonce : undefined,
    activity,
    loading: active && source.isPending,
    error:
      active && (source.isError || (source.data && !current && !unavailable))
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
