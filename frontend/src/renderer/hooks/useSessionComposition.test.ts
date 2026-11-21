import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { compositionFixture } from "../test/fixtures/rca-composition";
import { useSessionComposition } from "./useSessionComposition";

const client = vi.hoisted(() => ({ get: vi.fn(), set: vi.fn(), clear: vi.fn(), purge: vi.fn(), emergencyStop: vi.fn() }));
vi.mock("../lib/api-client", () => ({ compositionClient: client }));
beforeEach(() => {
	vi.clearAllMocks();
	client.get.mockResolvedValue(compositionFixture());
	for (const method of [client.set, client.clear, client.purge, client.emergencyStop]) method.mockResolvedValue({});
});

it("reads only the selected session without analyzing automatically", async () => {
	const { result } = renderHook(() => useSessionComposition("worker-1"));
	await waitFor(() => expect(result.current.view.status).toBe("ready"));
	expect(client.get).toHaveBeenCalledWith({ sessionId: "worker-1" });
});

it("discards a late response after switching sessions", async () => {
	let release: ((data: ReturnType<typeof compositionFixture>) => void) | undefined;
	client.get.mockImplementationOnce(
		() =>
			new Promise((resolve) => {
				release = resolve;
			}),
	);
	client.get.mockResolvedValueOnce({ ...compositionFixture(), sessionId: "worker-2" });
	const { result, rerender } = renderHook(({ id }) => useSessionComposition(id), { initialProps: { id: "worker-1" } });
	rerender({ id: "worker-2" });
	await waitFor(() => expect(result.current.view.status).toBe("ready"));
	await act(async () => release?.({ ...compositionFixture(), sessionId: "old-canary" }));
	if (result.current.view.status !== "ready") throw new Error("missing result");
	expect(result.current.view.data.sessionId).not.toBe("old-canary");
});

it("withdraws old numbers before a control completes and uses explicit analyze", async () => {
	const { result } = renderHook(() => useSessionComposition("worker-1"));
	await waitFor(() => expect(result.current.view.status).toBe("ready"));
	let release: (() => void) | undefined;
	client.set.mockImplementationOnce(
		() =>
			new Promise<void>((resolve) => {
				release = resolve;
			}),
	);
	let pending: Promise<void> | undefined;
	act(() => {
		pending = result.current.run("off");
	});
	expect(result.current.view.status).toBe("loading");
	await act(async () => {
		release?.();
		await pending;
	});
	expect(client.set).toHaveBeenCalledWith({ sessionId: "worker-1", enabled: false });
	await act(async () => result.current.run("analyze"));
	expect(client.get).toHaveBeenLastCalledWith({ sessionId: "worker-1", query: { analyze: true } });
});

it("does not expose transport error content", async () => {
	client.get.mockRejectedValue(new Error("A_FAKE_CREDENTIAL B_PRIVATE_PATH"));
	const { result } = renderHook(() => useSessionComposition("worker-1"));
	await waitFor(() => expect(result.current.view.status).toBe("error"));
	expect(JSON.stringify(result.current.view)).not.toMatch(/A_FAKE|B_PRIVATE/);
});

it("does not restore an old projection after off/on ABA", async () => {
	const { result } = renderHook(() => useSessionComposition("worker-1"));
	await waitFor(() => expect(result.current.view.status).toBe("ready"));
	let release: (() => void) | undefined;
	client.set.mockImplementationOnce(
		() =>
			new Promise<void>((resolve) => {
				release = resolve;
			}),
	);
	let old: Promise<void> | undefined;
	act(() => {
		old = result.current.run("off");
	});
	const fresh = compositionFixture();
	fresh.recorded = null;
	client.get.mockResolvedValue(fresh);
	await act(async () => result.current.run("on"));
	await act(async () => {
		release?.();
		await old;
	});
	if (result.current.view.status !== "ready") throw new Error("missing fresh view");
	expect(result.current.view.data.recorded).toBeNull();
	expect(client.get).toHaveBeenCalledTimes(2);
});
