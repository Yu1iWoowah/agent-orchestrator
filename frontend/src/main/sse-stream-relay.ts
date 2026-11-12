// Shared Electron-main SSE relay. Main opens an event stream with Node's
// global fetch and forwards the decoded chunks to the WebContents that opened
// it over a per-stream IPC channel. The cloud control-plane proxy and the
// local daemon stream proxy both use it.
//
// Node's fetch keeps these long-lived streams out of Chromium's connection
// pool (Electron `net` would share it): a renderer EventSource holds one of
// the six per-host connections for as long as it is open.

import { randomUUID } from "node:crypto";

export type SseRelayEvent =
	| { type: "chunk"; data: string }
	| { type: "end" }
	| { type: "error"; message: string };

/**
 * Structural slice of Electron.WebContents the relay needs. Kept minimal so
 * tests can drive streams with a plain object.
 */
export interface SseRelaySender {
	readonly id: number;
	isDestroyed(): boolean;
	send(channel: string, event: SseRelayEvent): void;
	once(event: "destroyed", listener: () => void): unknown;
	on(event: "render-process-gone", listener: () => void): unknown;
	on(event: "did-start-navigation", listener: (details: { isMainFrame: boolean; isSameDocument: boolean }) => void): unknown;
}

export interface SseRelayOptions {
	/** Per-stream renderer event channel. */
	channel(streamId: string): string;
	/** Upper bound on live streams per WebContents for this relay. */
	maxStreamsPerSender: number;
	/** Prefix of open failures, so the renderer can recover the HTTP status. */
	errorMarker: string;
	/** Noun used in the limit message, e.g. "cloud streams". */
	streamNoun: string;
}

export interface SseRelay {
	open(sender: SseRelaySender, connect: (signal: AbortSignal) => Promise<Response>): Promise<{ streamId: string }>;
	close(sender: Pick<SseRelaySender, "id">, streamId: unknown): void;
}

interface ActiveStream {
	controller: AbortController;
	sender: SseRelaySender;
	channel: string;
	closed: boolean;
}

/** openStream failures cross IPC as message strings; the marker keeps the status. */
export function sseRelayError(marker: string, status: number, detail: string): Error {
	return new Error(`${marker} ${status} ${detail}`);
}

async function envelopeMessage(response: Response): Promise<string> {
	const fallback = `The event stream request failed with status ${response.status}.`;
	try {
		const body = (await response.json()) as { message?: unknown; error?: unknown } | null;
		if (typeof body?.message === "string" && body.message !== "") return body.message;
		if (typeof body?.error === "string" && body.error !== "") return body.error;
	} catch {
		// Non-JSON or empty body: keep the status-derived message.
	}
	return fallback;
}

export function createSseRelay(options: SseRelayOptions): SseRelay {
	const streams = new Map<string, ActiveStream>();
	const streamIdsBySender = new Map<number, Set<string>>();

	function teardown(streamId: string, stream: ActiveStream): void {
		stream.closed = true;
		streams.delete(streamId);
		streamIdsBySender.get(stream.sender.id)?.delete(streamId);
		stream.controller.abort();
	}

	function emit(stream: ActiveStream, event: SseRelayEvent): void {
		if (stream.closed || stream.sender.isDestroyed()) return;
		stream.sender.send(stream.channel, event);
	}

	// A reload, a renderer crash, or a destroyed WebContents leaves nobody to
	// close the old page's streams. The error tells a page that survives (a
	// navigation that never committed) to reconnect.
	function releaseSender(senderId: number, message: string): void {
		for (const streamId of [...(streamIdsBySender.get(senderId) ?? [])]) {
			const stream = streams.get(streamId);
			if (stream === undefined) continue;
			emit(stream, { type: "error", message });
			teardown(streamId, stream);
		}
	}

	function trackSender(sender: SseRelaySender): Set<string> {
		let ids = streamIdsBySender.get(sender.id);
		if (ids === undefined) {
			ids = new Set();
			streamIdsBySender.set(sender.id, ids);
			const senderId = sender.id;
			sender.once("destroyed", () => {
				releaseSender(senderId, "The window was closed.");
				streamIdsBySender.delete(senderId);
			});
			sender.on("render-process-gone", () => releaseSender(senderId, "The renderer process exited."));
			sender.on("did-start-navigation", (details) => {
				if (details.isMainFrame && !details.isSameDocument) releaseSender(senderId, "The page navigated.");
			});
		}
		return ids;
	}

	async function pump(streamId: string, stream: ActiveStream, body: ReadableStream<Uint8Array>): Promise<void> {
		const reader = body.getReader();
		const decoder = new TextDecoder();
		try {
			for (;;) {
				const { done, value } = await reader.read();
				if (value !== undefined) {
					const data = decoder.decode(value, { stream: true });
					if (data !== "") emit(stream, { type: "chunk", data });
				}
				if (done) break;
			}
			const tail = decoder.decode();
			if (tail !== "") emit(stream, { type: "chunk", data: tail });
			emit(stream, { type: "end" });
		} catch (error) {
			emit(stream, {
				type: "error",
				message: error instanceof Error ? error.message : String(error),
			});
		} finally {
			teardown(streamId, stream);
			reader.releaseLock();
		}
	}

	async function open(
		sender: SseRelaySender,
		connect: (signal: AbortSignal) => Promise<Response>,
	): Promise<{ streamId: string }> {
		const ids = trackSender(sender);
		if (ids.size >= options.maxStreamsPerSender) {
			throw sseRelayError(
				options.errorMarker,
				429,
				`This window already has ${options.maxStreamsPerSender} open ${options.streamNoun}.`,
			);
		}
		const controller = new AbortController();
		const response = await connect(controller.signal);
		if (!response.ok) {
			throw sseRelayError(options.errorMarker, response.status, await envelopeMessage(response));
		}
		if (response.body === null) {
			throw sseRelayError(options.errorMarker, response.status, "The event stream response has no body.");
		}

		const streamId = randomUUID();
		const stream: ActiveStream = {
			controller,
			sender,
			channel: options.channel(streamId),
			closed: false,
		};
		streams.set(streamId, stream);
		ids.add(streamId);
		const body = response.body;
		// Start pumping on the next macrotask: the invoke reply for open is
		// dispatched from the handler's promise chain (microtasks), so deferring
		// guarantees the renderer learns the streamId — and subscribes — before
		// the first chunk event can be sent.
		setImmediate(() => {
			void pump(streamId, stream, body);
		});
		return { streamId };
	}

	function close(sender: Pick<SseRelaySender, "id">, streamId: unknown): void {
		if (typeof streamId !== "string") return;
		const stream = streams.get(streamId);
		// Only the WebContents that opened a stream may close it.
		if (stream === undefined || stream.sender.id !== sender.id) return;
		teardown(streamId, stream);
	}

	return { open, close };
}
