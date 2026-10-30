import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { components } from "../../api/schema";
import { usePinReviewerAgentSettings, useReviewerAgentSettings } from "./useReviewerAgentSettings";
import { reviewerAgentSettingsQueryKey } from "../lib/reviewer-agent-settings";

const api = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn(), host: vi.fn() }));
vi.mock("../lib/host-clients", () => ({ clientForSessionHost: api.host }));
const clients: QueryClient[] = [];
type Snapshot = components["schemas"]["ReviewerAgentSettingsResponse"];
const snapshot: Snapshot = { reviewId: "review-owner", harness: "codex", revision: 5, settings: { model: "before", effort: "low", fastMode: "off" }, activePass: false, applyMode: "next-pass", effectiveState: "unknown", reason: "reviewer_effective_unavailable" };

beforeEach(() => { api.get.mockReset().mockResolvedValue({ data: snapshot }); api.patch.mockReset(); api.host.mockReset().mockReturnValue({ GET: api.get, PATCH: api.patch }); });
afterEach(() => { cleanup(); for (const client of clients.splice(0)) client.clear(); });

it("pins only its captured host and never invalidates conversation history", async () => {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); clients.push(client);
	client.setQueryData(reviewerAgentSettingsQueryKey("review-owner", "host-A"), snapshot);
	client.setQueryData(reviewerAgentSettingsQueryKey("review-owner", "host-B"), snapshot);
	const invalidate = vi.spyOn(client, "invalidateQueries");
	let finish: ((response: { data: object }) => void) | undefined;
	api.patch.mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
	const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	const { result } = renderHook(() => usePinReviewerAgentSettings(), { wrapper });
	act(() => result.current.mutate({ reviewId: "review-owner", hostId: "host-A", key: "intent-A", body: { expectedRevision: 5, settings: { model: "next" } } }));
	await waitFor(() => expect(api.patch).toHaveBeenCalled());
	expect(client.getQueryData<Snapshot>(reviewerAgentSettingsQueryKey("review-owner", "host-A"))?.settings?.model).toBe("before");
	await act(async () => { finish?.({ data: { id: "pin", reviewId: "review-owner", revision: 6, phase: "pinned", applyMode: "next-pass", target: { model: "next", effort: "", fastMode: "off" } } }); });
	await waitFor(() => expect(result.current.isSuccess).toBe(true));
	expect(api.host).toHaveBeenCalledWith("host-A");
	expect(client.getQueryData<Snapshot>(reviewerAgentSettingsQueryKey("review-owner", "host-A"))?.settings?.model).toBe("next");
	expect(client.getQueryData<Snapshot>(reviewerAgentSettingsQueryKey("review-owner", "host-B"))?.settings?.model).toBe("before");
	expect(invalidate).toHaveBeenCalledExactlyOnceWith({ queryKey: reviewerAgentSettingsQueryKey("review-owner", "host-A") });
});

it("rejects a snapshot or pin receipt belonging to another owner", async () => {
	const client = new QueryClient(); clients.push(client);
	api.get.mockResolvedValue({ data: { ...snapshot, reviewId: "worker-owner" } });
	api.patch.mockResolvedValue({ data: { reviewId: "worker-owner", phase: "pinned", applyMode: "next-pass" } });
	const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
	const { result } = renderHook(() => ({ read: useReviewerAgentSettings("review-owner", "remote"), pin: usePinReviewerAgentSettings() }), { wrapper });
	await waitFor(() => expect(result.current.read.isError).toBe(true));
	act(() => result.current.pin.mutate({ reviewId: "review-owner", hostId: "remote", key: "key", body: { expectedRevision: 5, settings: { fastMode: "off" } } }));
	await waitFor(() => expect(result.current.pin.isError).toBe(true));
	expect(client.getQueryData(reviewerAgentSettingsQueryKey("review-owner", "remote"))).toBeUndefined();
});
