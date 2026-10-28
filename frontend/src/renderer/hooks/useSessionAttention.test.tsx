import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  AttentionUnavailable,
  useAttentionSelection,
  useSessionAttention,
} from "./useSessionAttention";

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
  get.mockReset();
  post.mockReset();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  post.mockResolvedValue({ response: { status: 204 } });
});
afterEach(() => {
  cleanup();
  client.clear();
});
describe("attention source identity", () => {
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
