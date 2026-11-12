// Electron-main relay for the local daemon's long-lived SSE streams.
//
// A renderer EventSource holds one of Chromium's six connections to the
// daemon host for as long as it is open, which left ordinary API requests two
// connections (any slow request then queued the whole UI). These streams are
// opened here instead and forwarded over IPC (see sse-stream-relay.ts).
//
// The renderer names only the stream path. Main supplies the daemon origin
// from the supervisor's port, and only the stream routes below are accepted.

import { ipcMain } from "electron";
import { createSseRelay, sseRelayError, type SseRelaySender } from "./sse-stream-relay";

export const DAEMON_STREAM_OPEN_CHANNEL = "daemonStream:open";
export const DAEMON_STREAM_CLOSE_CHANNEL = "daemonStream:close";
export const DAEMON_STREAM_ERROR_MARKER = "DAEMON_STREAM_ERROR";
export const MAX_DAEMON_STREAMS_PER_WEBCONTENTS = 8;

export function daemonStreamChannel(streamId: string): string {
	return `daemonStream:stream:${streamId}`;
}

const STREAM_PATHS = [
	/^\/api\/v1\/events$/,
	/^\/api\/v1\/agents\/codex\/accounts\/events$/,
	/^\/api\/v1\/notifications\/stream$/,
	/^\/api\/v1\/sessions\/[^/]+\/workspace\/events$/,
	/^\/api\/v1\/sessions\/[^/]+\/conversation\/side-chats\/[^/]+\/events$/,
];

export interface DaemonStreamInit {
	/** Stream path and optional query, e.g. "/api/v1/events?after=latest". */
	path: string;
	/** Sent as Last-Event-ID when a dropped stream reconnects. */
	lastEventId?: string;
}

export interface DaemonStreamProxy {
	openStream(sender: SseRelaySender, init: unknown): Promise<{ streamId: string }>;
	closeStream(sender: Pick<SseRelaySender, "id">, streamId: unknown): void;
}

function invalid(detail: string): Error {
	return new Error(`Invalid daemon stream request: ${detail}`);
}

function validateInit(init: unknown): DaemonStreamInit {
	if (typeof init !== "object" || init === null || Array.isArray(init)) {
		throw invalid("the request payload must be an object.");
	}
	const record = init as Record<string, unknown>;
	for (const key of Object.keys(record)) {
		if (key !== "path" && key !== "lastEventId") throw invalid(`unexpected field ${JSON.stringify(key)}.`);
	}
	const { path, lastEventId } = record;
	if (typeof path !== "string" || !path.startsWith("/")) throw invalid("path must be an absolute path.");
	if (lastEventId !== undefined && (typeof lastEventId !== "string" || /[\r\n\0]/.test(lastEventId))) {
		throw invalid("lastEventId must be a single-line string.");
	}
	return { path, lastEventId };
}

/** Resolve against the daemon origin and check the normalized path. */
function resolveStreamUrl(baseUrl: string, path: string): string {
	const base = new URL(baseUrl);
	const target = new URL(path, base);
	if (target.origin !== base.origin || !STREAM_PATHS.some((pattern) => pattern.test(target.pathname))) {
		throw invalid(`${JSON.stringify(path)} is not a daemon event stream.`);
	}
	return `${target.origin}${target.pathname}${target.search}`;
}

export function createDaemonStreamProxy(
	getDaemonBaseUrl: () => string | null,
	options: { fetchImpl?: typeof fetch } = {},
): DaemonStreamProxy {
	// Wrapped so the global fetch is never invoked detached from its realm.
	const doFetch: typeof fetch = options.fetchImpl ?? ((input, init) => fetch(input, init));
	const relay = createSseRelay({
		channel: daemonStreamChannel,
		maxStreamsPerSender: MAX_DAEMON_STREAMS_PER_WEBCONTENTS,
		errorMarker: DAEMON_STREAM_ERROR_MARKER,
		streamNoun: "daemon streams",
	});

	async function openStream(sender: SseRelaySender, init: unknown): Promise<{ streamId: string }> {
		const { path, lastEventId } = validateInit(init);
		const baseUrl = getDaemonBaseUrl();
		if (baseUrl === null) throw sseRelayError(DAEMON_STREAM_ERROR_MARKER, 503, "The AO daemon is not running.");
		const url = resolveStreamUrl(baseUrl, path);
		const headers = new Headers({ Accept: "text/event-stream" });
		if (lastEventId !== undefined) headers.set("Last-Event-ID", lastEventId);
		return relay.open(sender, (signal) => doFetch(url, { headers, signal }));
	}

	return { openStream, closeStream: relay.close };
}

export function installDaemonStreamProxy(getDaemonBaseUrl: () => string | null): void {
	const proxy = createDaemonStreamProxy(getDaemonBaseUrl);
	ipcMain.handle(DAEMON_STREAM_OPEN_CHANNEL, (event, init: unknown) => proxy.openStream(event.sender, init));
	ipcMain.on(DAEMON_STREAM_CLOSE_CHANNEL, (event, streamId: unknown) => {
		proxy.closeStream(event.sender, streamId);
	});
}
