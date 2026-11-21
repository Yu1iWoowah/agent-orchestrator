// EventSource stand-in for the local daemon's SSE streams. The desktop main
// process holds the HTTP stream (main/daemon-stream-proxy.ts) and forwards its
// chunks over IPC, so the stream does not take one of Chromium's six
// connections to the daemon host. This class reassembles the chunks with the
// shared SSE frame parser and exposes the EventSource surface the callers use:
// onopen/onerror/onmessage, addEventListener, close, readyState, and a
// reconnect that resends Last-Event-ID.

import type { DaemonStreamInit } from "../../main/daemon-stream-proxy";
import type { SseRelayEvent } from "../../main/sse-stream-relay";
import { createSseFrameParser, type CloudCpSseFrame } from "./cloud-cp/sse";

export interface DaemonStreamBridge {
	openStream(init: DaemonStreamInit): Promise<{ streamId: string }>;
	closeStream(streamId: string): void;
	onStreamEvent(streamId: string, listener: (event: SseRelayEvent) => void): () => void;
}

const CONNECTING = 0;
const OPEN = 1;
const CLOSED = 2;
// Chromium's EventSource reconnection time; the daemon sends no `retry:`.
export const DAEMON_EVENT_SOURCE_RECONNECT_MS = 3_000;
// Main rejects an open with this marker when it got an HTTP answer (or has no
// daemon to ask). EventSource fails such a connection instead of retrying.
const HTTP_FAILURE = /DAEMON_STREAM_ERROR \d+/;

function report(error: unknown): void {
	setTimeout(() => {
		throw error;
	}, 0);
}

export class DaemonEventSource extends EventTarget {
	readonly CONNECTING = CONNECTING;
	readonly OPEN = OPEN;
	readonly CLOSED = CLOSED;
	readyState: number = CONNECTING;
	onopen: ((event: Event) => void) | null = null;
	onerror: ((event: Event) => void) | null = null;
	onmessage: ((event: MessageEvent<string>) => void) | null = null;
	private lastEventId: string | undefined;
	private generation = 0;
	private streamId: string | undefined;
	private unsubscribe: (() => void) | undefined;
	private reconnectTimer: ReturnType<typeof setTimeout> | undefined;

	constructor(
		readonly url: string,
		private readonly bridge: DaemonStreamBridge,
	) {
		super();
		this.connect();
	}

	close(): void {
		this.readyState = CLOSED;
		this.release();
	}

	private connect(): void {
		const generation = this.generation;
		const init: DaemonStreamInit = { path: this.url };
		if (this.lastEventId !== undefined) init.lastEventId = this.lastEventId;
		this.bridge.openStream(init).then(
			({ streamId }) => {
				if (generation !== this.generation) {
					this.bridge.closeStream(streamId);
					return;
				}
				this.streamId = streamId;
				const parser = createSseFrameParser();
				// Subscribe in the same task as the open reply: main sends the first
				// chunk on a later macrotask.
				this.unsubscribe = this.bridge.onStreamEvent(streamId, (event) => {
					if (generation !== this.generation) return;
					if (event.type === "chunk") {
						for (const frame of parser.push(event.data)) {
							if (generation !== this.generation) return;
							this.deliver(frame);
						}
						return;
					}
					// The server ended the stream or it broke: EventSource reconnects.
					// An unterminated trailing frame is discarded, as EventSource does.
					this.reconnect();
				});
				this.readyState = OPEN;
				this.dispatch(new Event("open"));
			},
			(error: unknown) => {
				if (generation !== this.generation) return;
				const message = error instanceof Error ? error.message : String(error);
				if (HTTP_FAILURE.test(message)) this.fail();
				else this.reconnect();
			},
		);
	}

	private deliver(frame: CloudCpSseFrame): void {
		if (frame.id !== undefined) this.lastEventId = frame.id;
		this.dispatch(new MessageEvent(frame.event ?? "message", { data: frame.data, lastEventId: this.lastEventId ?? "" }));
	}

	private reconnect(): void {
		this.release();
		this.readyState = CONNECTING;
		this.dispatch(new Event("error"));
		if (this.readyState !== CONNECTING) return;
		const generation = this.generation;
		this.reconnectTimer = setTimeout(() => {
			this.reconnectTimer = undefined;
			if (generation === this.generation && this.readyState === CONNECTING) this.connect();
		}, DAEMON_EVENT_SOURCE_RECONNECT_MS);
	}

	private fail(): void {
		this.release();
		this.readyState = CLOSED;
		this.dispatch(new Event("error"));
	}

	/** Drop the current stream; later events from it are ignored. */
	private release(): void {
		this.generation += 1;
		if (this.reconnectTimer !== undefined) clearTimeout(this.reconnectTimer);
		this.reconnectTimer = undefined;
		this.unsubscribe?.();
		this.unsubscribe = undefined;
		if (this.streamId !== undefined) this.bridge.closeStream(this.streamId);
		this.streamId = undefined;
	}

	private dispatch(event: Event): void {
		const handler =
			event.type === "open" ? this.onopen : event.type === "error" ? this.onerror : event.type === "message" ? this.onmessage : null;
		try {
			handler?.call(this, event as MessageEvent<string>);
		} catch (error) {
			report(error);
		}
		this.dispatchEvent(event);
	}
}

/**
 * Open a local daemon stream through the desktop main process. Without the
 * desktop preload (browser preview, `dev:web`) there is no stream: callers keep
 * their polling path instead.
 */
export function openDaemonEventSource(path: string): DaemonEventSource | undefined {
	const bridge = daemonStreamBridge();
	return bridge === undefined ? undefined : new DaemonEventSource(path, bridge);
}

export function hasDaemonEventStreams(): boolean {
	return daemonStreamBridge() !== undefined;
}

function daemonStreamBridge(): DaemonStreamBridge | undefined {
	return typeof window === "undefined" ? undefined : window.ao?.daemonStream;
}
