import { QueryClient, QueryClientProvider, useInfiniteQuery } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { conversationQueryOptions, toSnapshot } from "./useConversation";
import { useAgentSettingsAction, useChangeAgentSettings } from "./useSessionAgentSettings";
import { agentSettingsOperation } from "../test/agent-settings-fixtures";

const { getMock, patchMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), patchMock: vi.fn(), postMock: vi.fn() }));
vi.mock("../lib/host-clients", async (importOriginal) => ({
	...await importOriginal<typeof import("../lib/host-clients")>(),
	clientForSessionHost: () => ({ GET: getMock, PATCH: patchMock, POST: postMock }),
}));

const clients: QueryClient[] = [];
const live = (model: string, oldestSequence = 401): Parameters<typeof toSnapshot>[0] => ({
	conversationId: "conversation-1", sessionId: "sess-1", harness: "codex", mode: "chat", controller: "ready",
	settings: { model }, turns: [], messages: [], activities: [], oldestSequence, latestSequence: oldestSequence + 199,
	hasMoreBefore: oldestSequence > 1, branchedFromEarlierMessage: false, continueTurnId: "", nativeForkAvailableAfterSequence: 0,
});

beforeEach(() => { getMock.mockReset(); patchMock.mockReset(); postMock.mockReset(); });
afterEach(() => { cleanup(); for (const client of clients.splice(0)) client.clear(); });

function setup() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	clients.push(client);
	const options = conversationQueryOptions("sess-1", "remote-a");
	const initialData = { pages: [toSnapshot(live("old-model")), toSnapshot(live("older", 201)), toSnapshot(live("oldest", 1))], pageParams: [undefined, 401, 201] };
	client.setQueryData(options.queryKey, initialData);
	const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	return { client, initialData, ...renderHook(() => ({
		conversation: useInfiniteQuery({ ...options, staleTime: Infinity, refetchOnMount: false, retry: false }),
		change: useChangeAgentSettings(), action: useAgentSettingsAction(),
	}), { wrapper }) };
}

it.each(["applied", "pending", "cancel"] as const)("refreshes only the remote live page after %s without waiting for historical downloads", async (phase) => {
	const cursors: (number | undefined)[] = [];
	getMock.mockImplementation((_path: string, options: { params: { query: { beforeSequence?: number } } }) => {
		cursors.push(options.params.query.beforeSequence);
		// An older-page download deliberately never completes. The mutation must not start it.
		return options.params.query.beforeSequence === undefined ? Promise.resolve({ data: live("new-model") }) : new Promise(() => {});
	});
	patchMock.mockResolvedValue({ data: { operation: agentSettingsOperation({ phase: phase === "cancel" ? "applied" : phase }) } });
	postMock.mockResolvedValue({ data: { operation: agentSettingsOperation({ phase: "cancelled" }) } });
	const { result, initialData } = setup();
	act(() => {
		if (phase === "cancel") result.current.action.mutate({ kind: "cancel", sessionId: "sess-1", hostId: "remote-a", operationId: "op-1", version: 1 });
		else result.current.change.mutate({ sessionId: "sess-1", hostId: "remote-a", key: "intent-1", body: { expectedRevision: 4, settings: { model: "new-model" } } });
	});
	await waitFor(() => expect(cursors.length).toBeGreaterThan(0));
	expect(cursors).toEqual([undefined]);
	await waitFor(() => expect((phase === "cancel" ? result.current.action : result.current.change).isSuccess).toBe(true));
	expect(result.current.conversation.data?.pages[0].settings.model).toBe("new-model");
	expect(result.current.conversation.data?.pages.slice(1)).toEqual(initialData.pages.slice(1));
	expect(result.current.conversation.data?.pageParams).toEqual(initialData.pageParams);
});

it.each(["change", "cancel"] as const)("keeps an accepted remote %s successful when the live refresh fails", async (kind) => {
	getMock.mockRejectedValue(new Error("Remote connection lost after acceptance"));
	patchMock.mockResolvedValue({ data: { operation: agentSettingsOperation({ phase: "applied" }) } });
	postMock.mockResolvedValue({ data: { operation: agentSettingsOperation({ phase: "cancelled" }) } });
	const { client, result, initialData } = setup();
	act(() => {
		if (kind === "cancel") result.current.action.mutate({ kind: "cancel", sessionId: "sess-1", hostId: "remote-a", operationId: "op-1", version: 1 });
		else result.current.change.mutate({ sessionId: "sess-1", hostId: "remote-a", key: "intent-1", body: { expectedRevision: 4, settings: { model: "new-model" } } });
	});
	await waitFor(() => expect((kind === "cancel" ? result.current.action : result.current.change).isSuccess).toBe(true));
	expect(getMock).toHaveBeenCalledTimes(1);
	expect(result.current.conversation.data).toEqual(initialData);
	expect(client.getQueryData(["remote-conversation-refresh-error", "remote-a", "sess-1"])).toBe(true);
});
