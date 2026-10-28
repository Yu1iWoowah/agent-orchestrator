import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { PlanUnavailable, useSessionPlan, useSessionPlanHistory } from "./useSessionPlan";

const { get } = vi.hoisted(() => ({ get: vi.fn() }));
vi.mock("../lib/host-clients", () => ({
	clientForSessionHost: (hostId?: string) => ({
		GET: (...args: unknown[]) => get(hostId, ...args),
	}),
}));
function wrapper() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return ({ children }: { children: ReactNode }) => (
		<QueryClientProvider client={client}>{children}</QueryClientProvider>
	);
}
describe("session Plan reads", () => {
	beforeEach(() => vi.clearAllMocks());
	it("does not show a late response from the previously selected session or host", async () => {
		let finish: (value: unknown) => void = () => {};
		get.mockImplementation((host) =>
			host === "a"
				? new Promise((resolve) => {
						finish = resolve;
					})
				: Promise.resolve({
						data: { state: "cleared", activeBranchId: "b" },
						response: { status: 200 },
					}),
		);
		const { result, rerender } = renderHook(({ host }) => useSessionPlan("same-session", host), {
			initialProps: { host: "a" },
			wrapper: wrapper(),
		});
		rerender({ host: "b" });
		await waitFor(() => expect(result.current.data?.state).toBe("cleared"));
		await act(async () =>
			finish({ data: { state: "present", activeBranchId: "a" }, response: { status: 200 } }),
		);
		expect(result.current.data?.activeBranchId).toBe("b");
	});
	it("treats an unsupported host as unavailable rather than absent", async () => {
		get.mockResolvedValue({ error: { code: "NOT_IMPLEMENTED" }, response: { status: 501 } });
		const { result } = renderHook(() => useSessionPlan("session"), { wrapper: wrapper() });
		await waitFor(() => expect(result.current.error).toBeInstanceOf(PlanUnavailable));
		expect(result.current.data).toBeUndefined();
	});
	it("refuses history returned for a branch that changed during the request", async () => {
		get.mockResolvedValue({
			data: { activeBranchId: "new-branch", versions: [], hasMoreBefore: false },
			response: { status: 200 },
		});
		const { result } = renderHook(
			() => useSessionPlanHistory("session", undefined, "old-branch", true),
			{ wrapper: wrapper() },
		);
		await waitFor(() => expect(result.current.isError).toBe(true));
		expect(result.current.data).toBeUndefined();
	});
	it("uses the history sequence cursor and stops after the last page", async () => {
		get.mockImplementation((_host, _path, request) => {
			const lastPage = request.params.query.beforeSequence === 20;
			return Promise.resolve({
				data: {
					activeBranchId: "root",
					versions: [{ sequence: lastPage ? 10 : 20 }],
					hasMoreBefore: !lastPage,
					nextBeforeSequence: lastPage ? undefined : 20,
				},
			});
		});
		const { result } = renderHook(() => useSessionPlanHistory("session", undefined, "root", true), {
			wrapper: wrapper(),
		});
		await waitFor(() => expect(result.current.hasNextPage).toBe(true));
		await act(async () => {
			await result.current.fetchNextPage();
		});
		expect(get).toHaveBeenLastCalledWith(
			undefined,
			"/api/v1/sessions/{sessionId}/plan/history",
			expect.objectContaining({
				params: { path: { sessionId: "session" }, query: { beforeSequence: 20, limit: 20 } },
			}),
		);
		await waitFor(() =>
			expect(
				result.current.data?.pages.flatMap((page) =>
					page.versions.map((version) => version.sequence),
				),
			).toEqual([20, 10]),
		);
		expect(result.current.hasNextPage).toBe(false);
	});
});
