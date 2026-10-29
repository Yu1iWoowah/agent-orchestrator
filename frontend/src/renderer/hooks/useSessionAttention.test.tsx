import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
	AttentionUnavailable,
	useAttentionSelection,
	useSessionAttention,
	sessionAttentionQueryKey,
} from "./useSessionAttention";
import { useProviderInputDraftStore } from "../stores/provider-input-draft-store";

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));
vi.mock("../lib/host-clients", () => ({
	clientForSessionHost: (host?: string) => ({
		GET: (path: string, request: unknown) => get(host, path, request),
		POST: (path: string, request: unknown) => post(host, path, request),
	}),
}));
let client: QueryClient;
const source = {
	branchId: "root",
	actionable: true,
	read: false,
	activity: {
		kind: "activity",
		id: "source",
		requestId: "provider-rpc",
		activityKind: "approval",
		status: "pending",
		sequence: 9,
		revision: 0,
		summary: "Approve scope",
		createdAt: "2026-10-29T00:00:00Z",
		detail: {
			decisions: [{ id: "allow", label: "Allow", kind: "allow_once" }],
		},
	},
};
const page = {
	conversationId: "conv",
	activeBranchId: "root",
	items: [source],
	counts: { open: 1, unread: 1, history: 0 },
	hasMoreBefore: false,
};
const wrapper = ({ children }: { children: ReactNode }) => (
	<QueryClientProvider client={client}>{children}</QueryClientProvider>
);
beforeEach(() => {
	useProviderInputDraftStore.setState({ drafts: {} });
	get.mockReset();
	post.mockReset();
	client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	post.mockResolvedValue({ response: { status: 204 } });
});
afterEach(() => {
	cleanup();
	client.clear();
	vi.useRealTimers();
});
describe("unselected provider input drafts", () => {
	function prepare(hostId?: string) {
		let closedA = false;
		let omitA = false;
		let lookup:
			| "current"
			| "offline"
			| "other-branch"
			| "other-conversation"
			| "older-revision" = "current";
		const key = (
			id: string,
			host = hostId,
			session = "session",
			branch = "root",
		) => JSON.stringify([host ?? null, session, "conv", branch, id, 2]);
		const retained = [
			key("A", "another-host"),
			key("A", hostId, "another-session"),
			key("A", hostId, "session", "another-branch"),
		];
		for (const draftKey of [key("A"), key("B"), ...retained]) {
			useProviderInputDraftStore.getState().save(draftKey, {
				values: { target: draftKey === key("A") ? "A-draft" : "B-draft" },
				activeQuestion: 0,
				collapsed: false,
			});
		}
		const item = (id: string) => ({
			...source,
			actionable: !(closedA && id === "A"),
			activity: {
				...source.activity,
				id,
				requestId: id,
				activityKind: "user_input",
				status: closedA && id === "A" ? "resolved" : "pending",
				revision: 2,
			},
		});
		get.mockImplementation(
			(
				_host,
				path: string,
				request: {
					params: { path: { activityId?: string }; query?: { state: string } };
				},
			) => {
				const isSource = path.endsWith("{activityId}");
				if (isSource && lookup === "offline")
					return Promise.reject(new Error("offline"));
				const items = isSource
					? [item(request.params.path.activityId ?? "")]
					: [item("A"), item("B")].filter((entry) =>
							request.params.query?.state === "history"
								? !entry.actionable
								: entry.actionable && (!omitA || entry.activity.id !== "A"),
						);
				return Promise.resolve({
					data: {
						...page,
						hasMoreBefore: omitA && !closedA,
						conversationId:
							isSource && lookup === "other-conversation"
								? "another-conversation"
								: "conv",
						activeBranchId:
							isSource && lookup === "other-branch" ? "another-branch" : "root",
						items:
							lookup === "older-revision" && isSource
								? items.map((entry) => ({
										...entry,
										activity: { ...entry.activity, revision: 1 },
									}))
								: items,
						counts: {
							open: closedA ? 1 : 2,
							unread: 0,
							history: closedA ? 1 : 0,
						},
					},
					response: { status: 200 },
				});
			},
		);
		const hook = renderHook(
			() => ({
				list: useSessionAttention("session", hostId, "root", "open"),
				selection: useAttentionSelection("session", hostId, "root", true),
			}),
			{ wrapper },
		);
		const refresh = async () => {
			await act(async () => {
				await client.invalidateQueries({
					queryKey: sessionAttentionQueryKey("session", hostId),
				});
			});
		};
		return {
			...hook,
			key,
			retained,
			refresh,
			hideA: () => {
				omitA = true;
			},
			closeA: () => {
				closedA = true;
			},
			setLookup: (value: typeof lookup) => {
				lookup = value;
			},
		};
	}

	it.each([undefined, "remote"])(
		"evicts externally resolved unselected A without history and retains pending B (host %s)",
		async (hostId) => {
			const hook = prepare(hostId);
			await waitFor(() =>
				expect(hook.result.current.list.isSuccess).toBe(true),
			);
			act(() => hook.result.current.selection.locate("A"));
			await waitFor(() =>
				expect(hook.result.current.selection.activity?.id).toBe("A"),
			);
			act(() => hook.result.current.selection.locate("B"));
			await waitFor(() =>
				expect(hook.result.current.selection.activity?.id).toBe("B"),
			);
			hook.closeA();
			await hook.refresh();
			await waitFor(() =>
				expect(hook.result.current.list.data?.pages[0].counts.open).toBe(1),
			);
			await waitFor(() =>
				expect(
					useProviderInputDraftStore.getState().drafts[hook.key("A")],
				).toBeUndefined(),
			);
			expect(hook.result.current.selection.selectedId).toBe("B");
			expect(
				useProviderInputDraftStore.getState().drafts[hook.key("B")]?.values
					.target,
			).toBe("B-draft");
			for (const key of hook.retained)
				expect(useProviderInputDraftStore.getState().drafts[key]).toBeDefined();
			expect(
				get.mock.calls.some(
					([, , request]) => request.params.query?.state === "history",
				),
			).toBe(false);
		},
	);

	it("keeps drafts when the source is pending, offline, out of scope or older than the draft", async () => {
		const hook = prepare();
		await waitFor(() => expect(hook.result.current.list.isSuccess).toBe(true));
		// A can be absent from an open page because of pagination, not resolution.
		hook.hideA();
		await hook.refresh();
		expect(
			useProviderInputDraftStore.getState().drafts[hook.key("A")]?.values
				.target,
		).toBe("A-draft");
		hook.closeA();
		for (const state of [
			"offline",
			"other-branch",
			"other-conversation",
			"older-revision",
		] as const) {
			hook.setLookup(state);
			await hook.refresh();
			expect(
				useProviderInputDraftStore.getState().drafts[hook.key("A")]?.values
					.target,
			).toBe("A-draft");
			expect(
				useProviderInputDraftStore.getState().drafts[hook.key("B")]?.values
					.target,
			).toBe("B-draft");
		}
		hook.setLookup("current");
		await hook.refresh();
		await waitFor(() =>
			expect(
				useProviderInputDraftStore.getState().drafts[hook.key("A")],
			).toBeUndefined(),
		);
		expect(
			useProviderInputDraftStore.getState().drafts[hook.key("B")]?.values
				.target,
		).toBe("B-draft");
	});

	it.each([undefined,"remote"])("refreshes a selected drafted source once (host %s)",async(hostId)=>{
		const hook=prepare(hostId);
		await waitFor(()=>expect(hook.result.current.list.isSuccess).toBe(true));
		act(()=>hook.result.current.selection.locate("B"));
		await waitFor(()=>expect(hook.result.current.selection.activity?.id).toBe("B"));
		get.mockClear();
		await hook.refresh();
		const sourceCalls=get.mock.calls.filter(([,path,request])=>path.endsWith("{activityId}") && request.params.path.activityId==="B");
		expect(sourceCalls).toHaveLength(1);
		expect(hook.result.current.selection.activity?.id).toBe("B");
		expect(useProviderInputDraftStore.getState().drafts[hook.key("B")]?.values.target).toBe("B-draft");
	});
});

it("does not expose a cached form while reopening or after a failed fresh lookup",async()=>{
	get.mockResolvedValue({data:page,response:{status:200}});
	const hook=renderHook(()=>useAttentionSelection("session",undefined,"root",true),{wrapper});
	act(()=>hook.result.current.locate("source"));
	await waitFor(()=>expect(hook.result.current.activity?.id).toBe("source"));
	get.mockRejectedValue(new Error("offline"));
	act(()=>hook.result.current.locate("source"));
	expect(hook.result.current.loading).toBe(true);
	expect(hook.result.current.activity).toBeUndefined();
	await waitFor(()=>expect(hook.result.current.error).toBe(true));
	expect(hook.result.current.activity).toBeUndefined();
	expect(hook.result.current.selectedId).toBe("source");
});

describe("attention source identity", () => {
	it("releases a source resolved by another client and removes only its scoped drafts", async () => {
		let resolved = false;
		get.mockImplementation(() =>
			Promise.resolve({
				data: {
					...page,
					items: [
						{
							...source,
							actionable: !resolved,
							activity: {
								...source.activity,
								activityKind: "user_input",
								status: resolved ? "resolved" : "pending",
							},
						},
					],
				},
				response: { status: 200 },
			}),
		);
		const key = (
			host: string | null,
			session: string,
			branch: string,
			id: string,
			revision = 0,
		) => JSON.stringify([host, session, "conv", branch, id, revision]);
		const matching = [
			key(null, "session", "root", "source"),
			key(null, "session", "root", "source", 2),
		];
		const retained = [
			key("remote", "session", "root", "source"),
			key(null, "another-session", "root", "source"),
			key(null, "session", "other-branch", "source"),
			key(null, "session", "root", "other-source"),
		];
		for (const draftKey of [...matching, ...retained])
			useProviderInputDraftStore
				.getState()
				.save(draftKey, {
					values: { target: "staging" },
					activeQuestion: 0,
					collapsed: false,
				});
		const { result } = renderHook(
			() => useAttentionSelection("session", undefined, "root", true),
			{ wrapper },
		);
		act(() => result.current.locate("source"));
		await waitFor(() => expect(result.current.activity).toBeDefined());
		resolved = true;
		await act(async () => {
			await client.invalidateQueries({
				queryKey: sessionAttentionQueryKey("session"),
			});
		});
		await waitFor(() => expect(result.current.selectedId).toBeUndefined());
		expect(result.current.unavailable).toBe(false);
		for (const draftKey of matching)
			expect(
				useProviderInputDraftStore.getState().drafts[draftKey],
			).toBeUndefined();
		for (const draftKey of retained)
			expect(
				useProviderInputDraftStore.getState().drafts[draftKey]?.values.target,
			).toBe("staging");
	});
	it("releases a source that has disappeared but keeps selection and drafts on a network failure", async () => {
		get.mockResolvedValue({ data: page, response: { status: 200 } });
		const { result } = renderHook(
			() => useAttentionSelection("session", undefined, "root", true),
			{ wrapper },
		);
		act(() => result.current.locate("source"));
		await waitFor(() => expect(result.current.activity).toBeDefined());
		get.mockRejectedValue(new Error("offline"));
		await act(async () => {
			await client.invalidateQueries({
				queryKey: sessionAttentionQueryKey("session"),
			});
		});
		await waitFor(() => expect(result.current.error).toBe(true));
		expect(result.current.selectedId).toBe("source");
		get.mockResolvedValue({
			error: { code: "ATTENTION_CHANGED" },
			response: { status: 409 },
		});
		await act(async () => {
			await client.invalidateQueries({
				queryKey: sessionAttentionQueryKey("session"),
			});
		});
		await waitFor(() => expect(result.current.selectedId).toBeUndefined());
	});
	it("does not let a late successful answer clear a newer selection", async () => {
		get.mockImplementation((_host, _path, request) =>
			Promise.resolve({
				data: {
					...page,
					items: [
						{
							...source,
							activity: {
								...source.activity,
								id: request.params.path.activityId,
							},
						},
					],
				},
				response: { status: 200 },
			}),
		);
		const { result } = renderHook(
			() => useAttentionSelection("session", undefined, "root", true),
			{ wrapper },
		);
		act(() => result.current.locate("A"));
		await waitFor(() => expect(result.current.activity?.id).toBe("A"));
		const answerACompleted = result.current.clear;
		act(() => result.current.locate("B"));
		await waitFor(() => expect(result.current.activity?.id).toBe("B"));
		act(() => answerACompleted());
		expect(result.current.selectedId).toBe("B");
		act(() => result.current.clear());
		await waitFor(() => expect(result.current.selectedId).toBeUndefined());
	});
	it("keeps a failed read scoped to its source when switching hosts", async () => {
		get.mockResolvedValue({ data: page, response: { status: 200 } });
		let failRead: ((value: unknown) => void) | undefined;
		post.mockImplementation(
			() =>
				new Promise((resolve) => {
					failRead = resolve;
				}),
		);
		const { result, rerender } = renderHook(
			({ host }: { host?: string }) =>
				useAttentionSelection("session", host, "root", true),
			{ wrapper, initialProps: {} },
		);
		act(() => result.current.locate("source"));
		await waitFor(() => expect(result.current.activity).toBeDefined());
		act(() => result.current.markViewed());
		await waitFor(() => expect(post).toHaveBeenCalledTimes(1));
		await act(async () => {
			failRead?.({ error: { message: "local read failed" } });
		});
		await waitFor(() => expect(result.current.readError).toBe(true));
		rerender({ host: "remote" });
		expect(result.current.activity).toBeUndefined();
		expect(result.current.readError).toBe(false);
	});
	it("fetches current source facts before selection and marks only an explicitly viewed revision read", async () => {
		get.mockResolvedValue({ data: page, response: { status: 200 } });
		const { result } = renderHook(
			() => useAttentionSelection("session", undefined, "root", true),
			{ wrapper },
		);
		expect(get).not.toHaveBeenCalled();
		act(() => result.current.locate("source"));
		await waitFor(() =>
			expect(result.current.activity?.requestId).toBe("provider-rpc"),
		);
		expect(result.current.activity?.decisions?.[0]?.id).toBe("allow");
		expect(post).not.toHaveBeenCalled();
		act(() => result.current.markViewed());
		await waitFor(() =>
			expect(post).toHaveBeenCalledWith(
				undefined,
				"/api/v1/sessions/{sessionId}/attention/{activityId}/read",
				expect.objectContaining({
					params: { path: { sessionId: "session", activityId: "source" } },
					body: { expectedBranchId: "root", expectedRevision: 0 },
				}),
			),
		);
	});
	it("rejects a resolved source and an intervening branch instead of making a cached form actionable", async () => {
		get.mockResolvedValueOnce({
			data: { ...page, activeBranchId: "other-branch" },
			response: { status: 200 },
		});
		const { result } = renderHook(
			() => useAttentionSelection("session", undefined, "root", true),
			{ wrapper },
		);
		act(() => result.current.locate("source"));
		await waitFor(() => expect(result.current.error).toBe(true));
		expect(result.current.activity).toBeUndefined();
		get.mockResolvedValue({
			data: {
				...page,
				items: [
					{
						...source,
						actionable: false,
						activity: { ...source.activity, status: "resolved" },
					},
				],
			},
			response: { status: 200 },
		});
		act(() => result.current.locate("source"));
		await waitFor(() => expect(result.current.loading).toBe(false));
		expect(result.current.activity).toBeUndefined();
		expect(post).not.toHaveBeenCalled();
	});
	it("does not apply a late local result after switching to the same session id on another host", async () => {
		let finish: ((value: unknown) => void) | undefined;
		get.mockImplementation(
			() =>
				new Promise((resolve) => {
					finish = resolve;
				}),
		);
		const { result, rerender } = renderHook(
			({ host }: { host?: string }) =>
				useAttentionSelection("session", host, "root", true),
			{ wrapper, initialProps: {} },
		);
		act(() => result.current.locate("source"));
		await waitFor(() => expect(get).toHaveBeenCalledTimes(1));
		rerender({ host: "remote" });
		await act(async () => {
			finish?.({ data: page, response: { status: 200 } });
		});
		expect(result.current.activity).toBeUndefined();
		expect(result.current.selectedId).toBeUndefined();
		expect(post).not.toHaveBeenCalled();
	});
});
describe("attention pagination", () => {
	it("uses host refresh invalidation without a second interval polling loop", async () => {
		get.mockResolvedValue({ data: page, response: { status: 200 } });
		const { result } = renderHook(
			() => ({
				list: useSessionAttention("session", "remote", "root", "open"),
				source: useAttentionSelection("session", "remote", "root", true),
			}),
			{ wrapper },
		);
		act(() => result.current.source.locate("source"));
		await waitFor(() =>
			expect(
				result.current.list.isSuccess && result.current.source.activity,
			).toBeTruthy(),
		);
		const initial = get.mock.calls.length;
		await act(async () => {
			await new Promise((resolve) => setTimeout(resolve, 2200));
		});
		expect(get).toHaveBeenCalledTimes(initial);
		await act(async () => {
			await client.invalidateQueries({
				queryKey: sessionAttentionQueryKey("session", "remote"),
			});
		});
		expect(get).toHaveBeenCalledTimes(initial + 2);
	});
	it("rejects a non-attention API response instead of crashing the frame", async () => {
		get.mockResolvedValue({
			data: { status: "ok" },
			response: { status: 200 },
		});
		const { result } = renderHook(
			() => useSessionAttention("session", undefined, undefined, "open"),
			{ wrapper },
		);
		await waitFor(() => expect(result.current.isError).toBe(true));
		expect(result.current.data).toBeUndefined();
	});
	it("passes beforeSequence to the second page and stops at hasMoreBefore=false", async () => {
		get.mockImplementation((_host, _path, request) =>
			Promise.resolve({
				data:
					request.params.query.beforeSequence === 9
						? {
								...page,
								items: [
									{
										...source,
										activity: { ...source.activity, id: "older", sequence: 3 },
									},
								],
							}
						: { ...page, hasMoreBefore: true, nextBeforeSequence: 9 },
				response: { status: 200 },
			}),
		);
		const { result } = renderHook(
			() => useSessionAttention("session", undefined, "root", "open"),
			{ wrapper },
		);
		await waitFor(() => expect(result.current.hasNextPage).toBe(true));
		await act(async () => {
			await result.current.fetchNextPage();
		});
		await waitFor(() => expect(result.current.hasNextPage).toBe(false));
		expect(
			result.current.data?.pages.flatMap((p) =>
				p.items.map((i) => i.activity.id),
			),
		).toEqual(["source", "older"]);
		expect(get).toHaveBeenLastCalledWith(
			undefined,
			"/api/v1/sessions/{sessionId}/attention",
			expect.objectContaining({
				params: {
					path: { sessionId: "session" },
					query: { state: "open", beforeSequence: 9, limit: 20 },
				},
			}),
		);
	});
	it("distinguishes an unsupported old host from a successful empty pending list", async () => {
		get.mockResolvedValue({
			error: { code: "NOT_FOUND" },
			response: { status: 404 },
		});
		const { result } = renderHook(
			() => useSessionAttention("session", "old-host", "root", "open"),
			{ wrapper },
		);
		await waitFor(() => expect(result.current.isError).toBe(true));
		expect(result.current.error).toBeInstanceOf(AttentionUnavailable);
		expect(result.current.data).toBeUndefined();
	});
});
