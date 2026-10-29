import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { sessionQuestionFixture } from "../test/fixtures/session-question";
import { QuestionsUnavailable, questionSourceQueryOptions, useSessionQuestions } from "./useSessionQuestions";
import { sessionAttentionQueryKey } from "./useSessionAttention";

const { get } = vi.hoisted(() => ({ get: vi.fn() }));
vi.mock("../lib/host-clients", () => ({ clientForSessionHost: (host?: string) => ({ GET: (path: string, request: unknown) => get(host, path, request) }) }));
let client: QueryClient;
const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
beforeEach(() => { get.mockReset(); client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); });
afterEach(() => { cleanup(); client.clear(); });

it("uses the correct sequence cursor and keeps complete grouped counts", async () => {
	get.mockImplementation((_host, _path, request) => {
		const older = request.params.query.beforeSequence === 20;
		return Promise.resolve({ data: { items: [sessionQuestionFixture({ id: older ? "ask-old" : "ask-new", sequence: older ? 10 : 20 })], open: 2, unread: 1, history: 0, hasMoreBefore: !older, nextBeforeSequence: older ? 0 : 20 }, response: { status: 200 } });
	});
	const hook = renderHook(() => useSessionQuestions("session", undefined, "open"), { wrapper });
	await waitFor(() => expect(hook.result.current.hasNextPage).toBe(true));
	await act(async () => { await hook.result.current.fetchNextPage(); });
	await waitFor(() => expect(hook.result.current.data?.pages.flatMap((page) => page.items).map((item) => item.id)).toEqual(["ask-new", "ask-old"]));
	expect(hook.result.current.hasNextPage).toBe(false);
	expect(get).toHaveBeenLastCalledWith(undefined, "/api/v1/sessions/{sessionId}/questions", expect.objectContaining({ params: { path: { sessionId: "session" }, query: { state: "open", beforeSequence: 20, limit: 20 } } }));
});

it("reports unsupported hosts explicitly", async () => {
	get.mockResolvedValue({ error: { code: "NOT_IMPLEMENTED" }, response: { status: 501 } });
	const hook = renderHook(() => useSessionQuestions("session", "remote", "open"), { wrapper });
	await waitFor(() => expect(hook.result.current.error).toBeInstanceOf(QuestionsUnavailable));
	expect(hook.result.current.data).toBeUndefined();
});

it("rejects a question returned for another origin session", async () => {
	get.mockResolvedValue({ data: sessionQuestionFixture({ originSessionId: "foreign" }), response: { status: 200 } });
	await expect(client.fetchQuery(questionSourceQueryOptions({ sessionId: "session", questionId: "ask-1" }))).rejects.toThrow();
});

it("uses attention invalidation for the new list without a parallel interval", async () => {
	get.mockResolvedValue({ data: { items: [], open: 0, unread: 0, history: 0, hasMoreBefore: false, nextBeforeSequence: 0 }, response: { status: 200 } });
	const hook = renderHook(() => useSessionQuestions("session", "remote", "open"), { wrapper });
	await waitFor(() => expect(hook.result.current.isSuccess).toBe(true));
	get.mockClear();
	await act(async () => { await client.invalidateQueries({ queryKey: sessionAttentionQueryKey("session", "remote") }); });
	expect(get).toHaveBeenCalledTimes(1);
});
