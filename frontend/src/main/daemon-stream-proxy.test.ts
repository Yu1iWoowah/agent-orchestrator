import { describe, expect, it, vi } from "vitest";

vi.mock("electron", () => ({ ipcMain: {} }));
vi.mock("./cloud-auth", () => ({ getCloudAccessToken: vi.fn(async () => "cloud-token") }));

import { createCloudCpProxy, cloudCpStreamChannel } from "./cloud-cp-proxy";
import { createDaemonStreamProxy, daemonStreamChannel, MAX_DAEMON_STREAMS_PER_WEBCONTENTS } from "./daemon-stream-proxy";
import type { SseRelayEvent, SseRelaySender } from "./sse-stream-relay";

function fakeSender(id = 1) {
	const handlers = new Map<string, Array<(details?: unknown) => void>>();
	const add = (event: string, listener: (details?: unknown) => void) => {
		handlers.set(event, [...(handlers.get(event) ?? []), listener]);
	};
	const sent: Array<{ channel: string; event: SseRelayEvent }> = [];
	const sender = {
		id,
		sent,
		isDestroyed: () => false,
		send: (channel: string, event: SseRelayEvent) => {
			sent.push({ channel, event });
		},
		once: add,
		on: add,
		fire: (event: string, details?: unknown) => {
			for (const listener of handlers.get(event) ?? []) listener(details);
		},
	};
	return sender as unknown as SseRelaySender & Pick<typeof sender, "sent" | "fire">;
}

/** A fetch whose responses stay open until the test pushes or the signal aborts. */
function fakeFetch() {
	const calls: Array<{ url: string; headers: Headers; signal: AbortSignal; push: (text: string) => void }> = [];
	const fetchImpl = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
		let controller!: ReadableStreamDefaultController<Uint8Array>;
		const body = new ReadableStream<Uint8Array>({ start: (c) => { controller = c; } });
		const signal = init!.signal!;
		signal.addEventListener("abort", () => controller.error(new Error("aborted")));
		calls.push({
			url: String(input),
			headers: new Headers(init?.headers),
			signal,
			push: (text) => controller.enqueue(new TextEncoder().encode(text)),
		});
		return new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
	});
	return { fetchImpl: fetchImpl as unknown as typeof fetch, calls };
}

const localProxy = (getBase: () => string | null = () => "http://127.0.0.1:4100") => {
	const fetch = fakeFetch();
	return { proxy: createDaemonStreamProxy(getBase, { fetchImpl: fetch.fetchImpl }), ...fetch };
};

describe("daemon stream proxy", () => {
	it("opens each local stream route on the supervisor's daemon port and forwards chunks", async () => {
		const { proxy, calls } = localProxy();
		const sender = fakeSender();
		const paths = [
			"/api/v1/events?after=latest",
			"/api/v1/agents/codex/accounts/events",
			"/api/v1/notifications/stream",
			"/api/v1/sessions/session%2Fa/workspace/events",
			"/api/v1/sessions/s1/conversation/side-chats/side-1/events",
		];
		for (const path of paths) await proxy.openStream(sender, { path });
		expect(calls.map((call) => call.url)).toEqual(paths.map((path) => `http://127.0.0.1:4100${path}`));

		const { streamId } = await proxy.openStream(sender, { path: "/api/v1/events", lastEventId: "41" });
		const call = calls.at(-1)!;
		expect(call.headers.get("last-event-id")).toBe("41");
		expect(call.headers.get("accept")).toBe("text/event-stream");
		call.push("id: 42\nevent: session_updated\ndata: {}\n\n");
		await vi.waitFor(() =>
			expect(sender.sent).toContainEqual({
				channel: daemonStreamChannel(streamId),
				event: { type: "chunk", data: "id: 42\nevent: session_updated\ndata: {}\n\n" },
			}),
		);
	});

	it("rejects other paths and renderer-supplied origins before fetching", async () => {
		const { proxy, fetchImpl } = localProxy();
		const sender = fakeSender();
		for (const init of [
			{ path: "/api/v1/sessions" },
			{ path: "/api/v1/events/../projects" },
			{ path: "/api/v1/sessions/a/workspace/events/../../../../projects" },
			{ path: "//evil.test/api/v1/events" },
			{ path: "http://evil.test/api/v1/events" },
			{ path: "/api/v1/events", baseUrl: "http://evil.test" },
			{ path: "/api/v1/events", lastEventId: "1\r\nX-Injected: 1" },
		]) {
			await expect(proxy.openStream(sender, init)).rejects.toThrow("DAEMON_STREAM_ERROR 400 Invalid daemon stream request");
		}
		expect(fetchImpl).not.toHaveBeenCalled();
	});

	it("reports a daemon that is not running", async () => {
		const { proxy, fetchImpl } = localProxy(() => null);
		await expect(proxy.openStream(fakeSender(), { path: "/api/v1/events" })).rejects.toThrow("DAEMON_STREAM_ERROR 503");
		expect(fetchImpl).not.toHaveBeenCalled();
	});

	it("releases a page's streams when it reloads or its renderer exits", async () => {
		const { proxy, calls } = localProxy();
		const sender = fakeSender();
		const first = await proxy.openStream(sender, { path: "/api/v1/events" });
		await proxy.openStream(sender, { path: "/api/v1/notifications/stream" });

		sender.fire("did-start-navigation", { isMainFrame: true, isSameDocument: true });
		sender.fire("did-start-navigation", { isMainFrame: false, isSameDocument: false });
		expect(calls.map((call) => call.signal.aborted)).toEqual([false, false]);

		sender.fire("did-start-navigation", { isMainFrame: true, isSameDocument: false });
		expect(calls.map((call) => call.signal.aborted)).toEqual([true, true]);
		expect(sender.sent).toContainEqual({ channel: daemonStreamChannel(first.streamId), event: { type: "error", message: "The page navigated." } });

		await proxy.openStream(sender, { path: "/api/v1/events" });
		sender.fire("render-process-gone");
		expect(calls[2].signal.aborted).toBe(true);
	});

	it("aborts an open still waiting for headers when the page reloads or its renderer exits", async () => {
		for (const [event, details] of [
			["did-start-navigation", { isMainFrame: true, isSameDocument: false }],
			["render-process-gone", undefined],
		] as const) {
			const signals: AbortSignal[] = [];
			let respond!: () => void;
			const headers = new Promise<void>((resolve) => { respond = resolve; });
			// Ignores the signal, as a server that answers late would.
			const fetchImpl = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
				signals.push(init!.signal!);
				await headers;
				return new Response(new ReadableStream<Uint8Array>(), { status: 200 });
			});
			const proxy = createDaemonStreamProxy(() => "http://127.0.0.1:4100", { fetchImpl: fetchImpl as unknown as typeof fetch });
			const sender = fakeSender();
			const opening = Array.from({ length: MAX_DAEMON_STREAMS_PER_WEBCONTENTS }, () => proxy.openStream(sender, { path: "/api/v1/events" }));
			await vi.waitFor(() => expect(signals).toHaveLength(MAX_DAEMON_STREAMS_PER_WEBCONTENTS));

			sender.fire(event, details);
			expect(signals.every((signal) => signal.aborted)).toBe(true);
			respond();
			for (const open of opening) await expect(open).rejects.toThrow("released while it was opening");

			// None of them holds a slot: the new page can open a full set.
			for (let index = 0; index < MAX_DAEMON_STREAMS_PER_WEBCONTENTS; index += 1) {
				await proxy.openStream(sender, { path: "/api/v1/events" });
			}
		}
	});

	it("counts opens still in flight against the per-window cap", async () => {
		let respond!: () => void;
		const headers = new Promise<void>((resolve) => { respond = resolve; });
		const fetchImpl = vi.fn(async () => {
			await headers;
			return new Response(new ReadableStream<Uint8Array>(), { status: 200 });
		});
		const proxy = createDaemonStreamProxy(() => "http://127.0.0.1:4100", { fetchImpl: fetchImpl as unknown as typeof fetch });
		const sender = fakeSender();
		const opening = Array.from({ length: MAX_DAEMON_STREAMS_PER_WEBCONTENTS + 1 }, () => proxy.openStream(sender, { path: "/api/v1/events" }));
		await expect(opening.at(-1)).rejects.toThrow("DAEMON_STREAM_ERROR 429");
		respond();
		await Promise.all(opening.slice(0, -1));
		expect(fetchImpl).toHaveBeenCalledTimes(MAX_DAEMON_STREAMS_PER_WEBCONTENTS);
		await expect(proxy.openStream(sender, { path: "/api/v1/events" })).rejects.toThrow("DAEMON_STREAM_ERROR 429");
	});

	it("counts local streams apart from cloud streams, which keep their bearer", async () => {
		const local = localProxy();
		const cloudFetch = fakeFetch();
		const cloud = createCloudCpProxy(() => "/data", { fetchImpl: cloudFetch.fetchImpl });
		const sender = fakeSender();

		for (let index = 0; index < MAX_DAEMON_STREAMS_PER_WEBCONTENTS; index += 1) {
			await local.proxy.openStream(sender, { path: "/api/v1/events" });
		}
		await expect(local.proxy.openStream(sender, { path: "/api/v1/events" })).rejects.toThrow("DAEMON_STREAM_ERROR 429");

		const { streamId } = await cloud.openStream(sender, {
			baseUrl: "https://cp.test",
			path: "/api/cloud/v1/orgs/o/sessions/s/events",
			method: "GET",
			headers: { Accept: "text/event-stream", Authorization: "Bearer renderer" },
		});
		expect(cloudFetch.calls[0].url).toBe("https://cp.test/api/cloud/v1/orgs/o/sessions/s/events");
		expect(cloudFetch.calls[0].headers.get("authorization")).toBe("Bearer cloud-token");
		cloudFetch.calls[0].push("data: {}\n\n");
		await vi.waitFor(() =>
			expect(sender.sent).toContainEqual({ channel: cloudCpStreamChannel(streamId), event: { type: "chunk", data: "data: {}\n\n" } }),
		);
	});
});
