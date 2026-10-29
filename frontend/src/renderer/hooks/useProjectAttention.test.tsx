import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, renderHook, waitFor, act } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { useProjectAttention } from "./useProjectAttention";
const { get } = vi.hoisted(() => ({ get: vi.fn() }));
vi.mock("../lib/host-clients", () => ({
	clientForSessionHost: (host?: string) => ({
		GET: (path: string, request: unknown) => get(host, path, request),
	}),
}));
const clients: QueryClient[] = [];
afterEach(() => {
	cleanup();
	clients.forEach((client) => client.clear());
	get.mockReset();
});
function setup(
	project = "project",
	host?: string,
	audience: "user" | "orchestrator" = "user",
) {
	const client = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	clients.push(client);
	return renderHook(() => useProjectAttention(project, host, audience), {
		wrapper: ({ children }: { children: ReactNode }) => (
			<QueryClientProvider client={client}>{children}</QueryClientProvider>
		),
	});
}
it("keeps complete counts while paging with the server cursor and explicit audience/host", async () => {
	get
		.mockResolvedValueOnce({
			data: { items: [], open: 30, unread: 24, nextCursor: "page-2" },
			response: { status: 200 },
		})
		.mockResolvedValueOnce({
			data: { items: [], open: 30, unread: 24 },
			response: { status: 200 },
		});
	const { result } = setup("project", "remote", "orchestrator");
	await waitFor(() => expect(result.current.isSuccess).toBe(true));
	expect(result.current.data?.pages[0]?.open).toBe(30);
	expect(result.current.hasNextPage).toBe(true);
	await act(() => result.current.fetchNextPage());
	await waitFor(() => expect(result.current.data?.pages[1]?.open).toBe(30));
	expect(get.mock.calls[1]).toEqual([
		"remote",
		"/api/v1/projects/{id}/attention",
		expect.objectContaining({
			params: {
				path: { id: "project" },
				query: { audience: "orchestrator", cursor: "page-2", limit: 20 },
			},
		}),
	]);
	expect(result.current.hasNextPage).toBe(false);
});
it("treats an unavailable host as unknown rather than an empty count", async () => {
	get.mockRejectedValue(new Error("offline"));
	const { result } = setup();
	await waitFor(() => expect(result.current.isError).toBe(true));
	expect(result.current.data).toBeUndefined();
});
it("rejects malformed counts instead of presenting them as operational truth", async () => {
	get.mockResolvedValue({
		data: { items: [], open: -1, unread: 0 },
		response: { status: 200 },
	});
	const { result } = setup();
	await waitFor(() => expect(result.current.isError).toBe(true));
});
