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
});
