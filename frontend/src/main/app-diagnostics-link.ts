import { StringDecoder } from "node:string_decoder";
import { connect, type NetConnectOpts } from "node:net";
import { diagnosticId, isAppSnapshot, type AppSnapshot } from "../shared/app-diagnostics";

/** Keep the legacy first line so an older daemon still authenticates Browser. */
export function privateRuntimeHandoff(browserToken: string, appDiagnosticsToken: string): string {
	return `${browserToken}\n${JSON.stringify({ version: 1, browserToken, appDiagnosticsToken })}\n`;
}
export type DiagnosticsLinkOptions = {
	windows?: () => { id: string; generation: number }[];
	capture?: (request: { windowId?: string; projectId?: string }, signal: AbortSignal) => Promise<AppSnapshot>;
	state?: (state: "connected" | "disconnected" | "rejected") => void;
};
const safeCodes = new Set([
	"APP_DIAGNOSTICS_DISABLED",
	"APP_DIAGNOSTICS_BUSY",
	"APP_DIAGNOSTICS_INVALID_PAYLOAD",
	"APP_DIAGNOSTICS_PAYLOAD_TOO_LARGE",
	"APP_WINDOW_GONE",
	"APP_RENDERER_CHANGED",
	"APP_RENDERER_UNAVAILABLE",
	"APP_SNAPSHOT_TIMEOUT",
]);
function captureRequest(value: unknown): value is { windowId?: string; projectId?: string } {
	return (
		typeof value === "object" &&
		value !== null &&
		!Array.isArray(value) &&
		Object.keys(value).every((key) => (key === "windowId" || key === "projectId") && diagnosticId(Reflect.get(value, key)))
	);
}

/** A handle owns retry/backoff, but never restores native consent after disconnect. */
export function connectAppDiagnostics(address: NetConnectOpts, token: string, instanceId: string, options: DiagnosticsLinkOptions = {}) {
	let disposed = false;
	let retryTimer: ReturnType<typeof setTimeout> | undefined;
	let heartbeat: ReturnType<typeof setInterval> | undefined;
	let socket: ReturnType<typeof connect> | undefined;
	let refresh = () => undefined as void;
	let backoff = 200;
	const open = () => {
		if (disposed) return;
		const current = connect(address);
		socket = current;
		let buffer = "";
		let authenticated = false;
		let terminal = false;
		let lastWindows = "[]";
		const decoder = new StringDecoder("utf8");
		const pending = new Map<string, AbortController>();
		const seen = new Set<string>();
		const send = (value: unknown) => {
			if (!current.destroyed && socket === current) current.write(`${JSON.stringify(value)}\n`);
		};
		refresh = () => {
			if (!authenticated || disposed) return;
			const windows = options.capture ? (options.windows?.() ?? []) : [];
			const next = JSON.stringify(windows);
			if (next !== lastWindows) {
				lastWindows = next;
				send({ type: "ready", windows });
			}
		};
		heartbeat = setInterval(() => {
			if (authenticated) send({ type: "ping" });
		}, 5_000);
		current.once("connect", () => send({ type: "hello", version: 1, token, instanceId }));
		current.on("data", (chunk: Buffer) => {
			buffer += decoder.write(chunk);
			if (Buffer.byteLength(buffer) > 1 << 20) {
				current.destroy();
				return;
			}
			for (;;) {
				const index = buffer.indexOf("\n");
				if (index < 0) break;
				const raw = buffer.slice(0, index);
				buffer = buffer.slice(index + 1);
				let message: unknown;
				try {
					message = JSON.parse(raw);
				} catch {
					current.destroy();
					return;
				}
				if (typeof message !== "object" || message === null || !("type" in message)) {
					current.destroy();
					return;
				}
				if (message.type === "hello-rejected") {
					terminal = "code" in message && message.code === "APP_DIAGNOSTICS_CAPABILITY_INVALID";
					if (terminal) options.state?.("rejected");
					current.destroy();
					return;
				}
				if (
					message.type === "hello-ok" &&
					"version" in message &&
					message.version === 1 &&
					"epoch" in message &&
					diagnosticId(message.epoch)
				) {
					authenticated = true;
					backoff = 200;
					options.state?.("connected");
					refresh();
					continue;
				}
				if (authenticated && message.type === "pong") continue;
				if (authenticated && message.type === "cancel" && "requestId" in message && typeof message.requestId === "string") {
					pending.get(message.requestId)?.abort();
					continue;
				}
				if (
					authenticated &&
					message.type === "snapshot" &&
					"requestId" in message &&
					typeof message.requestId === "string" &&
					diagnosticId(message.requestId)
				) {
					const requestId = message.requestId;
					if (seen.has(requestId) || !Object.keys(message).every((key) => ["type", "requestId", "request"].includes(key))) {
						current.destroy();
						return;
					}
					if (seen.size >= 1024) seen.clear();
					seen.add(requestId);
					if (!options.capture || !("request" in message) || !captureRequest(message.request)) {
						send({ type: "result", requestId, code: "APP_DIAGNOSTICS_UNSUPPORTED" });
						continue;
					}
					const abort = new AbortController();
					pending.set(requestId, abort);
					void options
						.capture(message.request, abort.signal)
						.then((snapshot) => {
							if (disposed || socket !== current || current.destroyed || abort.signal.aborted) return;
							if (isAppSnapshot(snapshot) && snapshot.instanceId === instanceId) send({ type: "result", requestId, snapshot });
							else send({ type: "result", requestId, code: "APP_DIAGNOSTICS_INVALID_PAYLOAD" });
						})
						.catch((error: unknown) => {
							if (!disposed && !current.destroyed && !abort.signal.aborted)
								send({
									type: "result",
									requestId,
									code: error instanceof Error && safeCodes.has(error.message) ? error.message : "APP_RENDERER_UNAVAILABLE",
								});
						})
						.finally(() => pending.delete(requestId));
					continue;
				}
				current.destroy();
				return;
			}
		});
		current.on("error", () => current.destroy());
		current.once("close", () => {
			clearInterval(heartbeat);
			for (const abort of pending.values()) abort.abort();
			pending.clear();
			if (!terminal) options.state?.("disconnected");
			if (disposed || terminal) return;
			retryTimer = setTimeout(open, backoff);
			backoff = Math.min(backoff * 2, 2_000);
		});
	};
	open();
	const dispose = () => {
		disposed = true;
		clearTimeout(retryTimer);
		clearInterval(heartbeat);
		socket?.destroy();
	};
	return Object.assign(dispose, { refresh: () => refresh() });
}
