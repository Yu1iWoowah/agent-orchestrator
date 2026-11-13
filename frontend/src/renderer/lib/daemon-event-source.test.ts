import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { SseRelayEvent } from "../../main/sse-stream-relay";
import {
	DAEMON_EVENT_SOURCE_RECONNECT_MS,
	DaemonEventSource,
	type DaemonStreamBridge,
	hasDaemonEventStreams,
	openDaemonEventSource,
} from "./daemon-event-source";

function fakeBridge() {
	const listeners = new Map<string, (event: SseRelayEvent) => void>();
	let opened = 0;
	const bridge = {
		openStream: vi.fn(async () => ({ streamId: `stream-${(opened += 1)}` })),
		closeStream: vi.fn(),
		onStreamEvent: vi.fn((streamId: string, listener: (event: SseRelayEvent) => void) => {
			listeners.set(streamId, listener);
			return () => listeners.delete(streamId);
		}),
		emit: (streamId: string, event: SseRelayEvent) => listeners.get(streamId)?.(event),
	};
	return bridge satisfies DaemonStreamBridge;
}

const settle = () => vi.advanceTimersByTimeAsync(0);

beforeEach(() => {
	vi.useFakeTimers();
});

afterEach(() => {
	vi.useRealTimers();
});

describe("DaemonEventSource", () => {
	it("delivers named and unnamed events, then reconnects with Last-Event-ID after the stream drops", async () => {
		const bridge = fakeBridge();
		const source = new DaemonEventSource("/api/v1/events?after=latest", bridge);
		const onopen = vi.fn();
		const onerror = vi.fn();
		const onmessage = vi.fn();
		const updates: MessageEvent<string>[] = [];
		source.onopen = onopen;
		source.onerror = onerror;
		source.onmessage = onmessage;
		source.addEventListener("session_updated", (event) => updates.push(event as MessageEvent<string>));
		expect(source.readyState).toBe(source.CONNECTING);

		await settle();
		expect(bridge.openStream).toHaveBeenCalledWith({ path: "/api/v1/events?after=latest" });
		expect(source.readyState).toBe(source.OPEN);
		expect(onopen).toHaveBeenCalledTimes(1);

		// A frame split across chunks is delivered once it completes.
		bridge.emit("stream-1", { type: "chunk", data: "id: 7\nevent: session_updated\nda" });
		bridge.emit("stream-1", { type: "chunk", data: "ta: {\"a\":1}\n\n: keepalive\n\ndata: plain\n\n" });
		expect(updates.map((event) => [event.data, event.lastEventId])).toEqual([["{\"a\":1}", "7"]]);
		expect(onmessage.mock.calls[0][0].data).toBe("plain");

		bridge.emit("stream-1", { type: "end" });
		expect(source.readyState).toBe(source.CONNECTING);
		expect(onerror).toHaveBeenCalledTimes(1);
		expect(bridge.closeStream).toHaveBeenCalledWith("stream-1");

		await vi.advanceTimersByTimeAsync(DAEMON_EVENT_SOURCE_RECONNECT_MS);
		expect(bridge.openStream).toHaveBeenLastCalledWith({ path: "/api/v1/events?after=latest", lastEventId: "7" });
		expect(source.readyState).toBe(source.OPEN);
		expect(onopen).toHaveBeenCalledTimes(2);

		// The dropped stream's late events are ignored.
		bridge.emit("stream-1", { type: "chunk", data: "event: session_updated\ndata: stale\n\n" });
		expect(updates).toHaveLength(1);
	});

	it("retries a network failure but fails an HTTP error for good", async () => {
		const bridge = fakeBridge();
		bridge.openStream.mockRejectedValueOnce(new Error("fetch failed"));
		const retried = new DaemonEventSource("/api/v1/notifications/stream", bridge);
		await settle();
		expect(retried.readyState).toBe(retried.CONNECTING);
		await vi.advanceTimersByTimeAsync(DAEMON_EVENT_SOURCE_RECONNECT_MS);
		expect(retried.readyState).toBe(retried.OPEN);

		bridge.openStream.mockRejectedValueOnce(new Error("Error invoking remote method: DAEMON_STREAM_ERROR 400 bad cursor"));
		const failed = new DaemonEventSource("/api/v1/events?after=latest", bridge);
		const onerror = vi.fn(() => expect(failed.readyState).toBe(failed.CLOSED));
		failed.onerror = onerror;
		await settle();
		await vi.advanceTimersByTimeAsync(DAEMON_EVENT_SOURCE_RECONNECT_MS * 2);
		expect(onerror).toHaveBeenCalledTimes(1);
		expect(bridge.openStream).toHaveBeenCalledTimes(3);
	});

	it("fails for good when main rejects the request as invalid", async () => {
		const bridge = fakeBridge();
		bridge.openStream.mockRejectedValueOnce(
			new Error("Error invoking remote method 'daemonStream:open': Error: DAEMON_STREAM_ERROR 400 Invalid daemon stream request: \"/api/v1/x\" is not a daemon event stream."),
		);
		const source = new DaemonEventSource("/api/v1/x", bridge);
		const onerror = vi.fn();
		source.onerror = onerror;
		await settle();
		await vi.advanceTimersByTimeAsync(DAEMON_EVENT_SOURCE_RECONNECT_MS * 3);
		expect(source.readyState).toBe(source.CLOSED);
		expect(onerror).toHaveBeenCalledTimes(1);
		expect(bridge.openStream).toHaveBeenCalledTimes(1);
	});

	it("closes the stream, including one still opening", async () => {
		const bridge = fakeBridge();
		const open = new DaemonEventSource("/api/v1/events", bridge);
		const listener = vi.fn();
		open.addEventListener("session_updated", listener);
		await settle();
		open.close();
		expect(open.readyState).toBe(open.CLOSED);
		expect(bridge.closeStream).toHaveBeenCalledWith("stream-1");
		bridge.emit("stream-1", { type: "chunk", data: "event: session_updated\ndata: {}\n\n" });
		expect(listener).not.toHaveBeenCalled();

		const opening = new DaemonEventSource("/api/v1/events", bridge);
		opening.close();
		await settle();
		expect(bridge.closeStream).toHaveBeenCalledWith("stream-2");
		expect(bridge.onStreamEvent).toHaveBeenCalledTimes(1);
	});
});

describe("openDaemonEventSource", () => {
	it("opens nothing without the desktop bridge", () => {
		expect(window.ao?.daemonStream).toBeUndefined();
		expect(hasDaemonEventStreams()).toBe(false);
		expect(openDaemonEventSource("/api/v1/events")).toBeUndefined();
	});

	it("opens through the preload bridge when it is present", async () => {
		const bridge = fakeBridge();
		window.ao!.daemonStream = bridge;
		try {
			const source = openDaemonEventSource("/api/v1/notifications/stream");
			await settle();
			expect(source?.readyState).toBe(1);
			expect(bridge.openStream).toHaveBeenCalledWith({ path: "/api/v1/notifications/stream" });
			source?.close();
		} finally {
			window.ao!.daemonStream = undefined as never;
		}
	});
});
