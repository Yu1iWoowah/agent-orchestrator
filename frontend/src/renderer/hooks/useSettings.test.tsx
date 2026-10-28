import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { getMock, patchMock } = vi.hoisted(() => ({ getMock: vi.fn(), patchMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, PATCH: patchMock },
	apiErrorMessage: () => "request failed",
}));

import { useSettings, useUpdateReplyLanguage } from "./useSettings";

function wrapper(queryClient: QueryClient) {
	return ({ children }: { children: ReactNode }) => (
		<QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
	);
}

beforeEach(() => {
	getMock.mockReset();
	patchMock.mockReset();
});

describe("useSettings tracker intake gate", () => {
	it.each([
		[true, true],
		[false, false],
		[undefined, false],
	])("maps trackerIntakeEnabled=%s to %s", async (trackerIntakeEnabled, expected) => {
		getMock.mockResolvedValue({
			data: { trackerIntakeEnabled },
			error: undefined,
		});
		const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const { result } = renderHook(() => useSettings(), { wrapper: wrapper(queryClient) });

		await waitFor(() => expect(result.current.settings?.trackerIntakeEnabled).toBe(expected));
		expect(getMock).toHaveBeenCalledWith("/api/v1/settings");
	});
});

describe("agent reply language", () => {
	it.each(["zh-TW", "", undefined])("reads %s from the daemon", async (replyLanguage) => {
		getMock.mockResolvedValue({ data: { replyLanguage } });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const { result } = renderHook(() => useSettings(), { wrapper: wrapper(client) });
		await waitFor(() => expect(result.current.settings?.replyLanguage).toBe(replyLanguage ?? ""));
	});
	it.each(["zh-TW", ""] as const)("persists %s and refreshes settings", async (replyLanguage) => {
		patchMock.mockResolvedValue({ data: { replyLanguage } });
		const client = new QueryClient();
		const invalidate = vi.spyOn(client, "invalidateQueries");
		const { result } = renderHook(() => useUpdateReplyLanguage(), { wrapper: wrapper(client) });
		act(() => result.current.update(replyLanguage));
		await waitFor(() => expect(invalidate).toHaveBeenCalledWith({ queryKey: ["settings"] }));
		expect(patchMock).toHaveBeenCalledWith("/api/v1/settings/reply-language", { body: { replyLanguage } });
	});
	it("reports a rejected save without claiming persistence", async () => {
		patchMock.mockResolvedValue({ error: { message: "unavailable" } });
		const client = new QueryClient();
		const invalidate = vi.spyOn(client, "invalidateQueries");
		const { result } = renderHook(() => useUpdateReplyLanguage(), { wrapper: wrapper(client) });
		act(() => result.current.update("zh-TW"));
		await waitFor(() => expect(result.current.error).toBe("request failed"));
		expect(invalidate).not.toHaveBeenCalled();
	});
});
