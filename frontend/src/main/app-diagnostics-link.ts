import { StringDecoder } from "node:string_decoder";
import { connect, type NetConnectOpts } from "node:net";

/** Keep the legacy first line so an older daemon still authenticates Browser. */
export function privateRuntimeHandoff(browserToken: string, appDiagnosticsToken: string): string {
	return `${browserToken}\n${JSON.stringify({ version: 1, browserToken, appDiagnosticsToken })}\n`;
}

/** PR1 has no collector or consent UI. It never advertises ready or issues grants. */
export function connectAppDiagnostics(address: NetConnectOpts, token: string, instanceId: string): () => void {
	let disposed = false;
	let retryTimer: ReturnType<typeof setTimeout> | undefined;
	let heartbeat: ReturnType<typeof setInterval> | undefined;
	let socket: ReturnType<typeof connect> | undefined;
	let backoff = 200;
	const open = () => {
		if (disposed) return;
		const current = connect(address);
		socket = current;
		let buffer = "";
		let authenticated = false;
		let terminal = false;
		const decoder = new StringDecoder("utf8");
		const send = (value: unknown) => {
			if (!current.destroyed) current.write(`${JSON.stringify(value)}\n`);
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
					current.destroy();
					return;
				}
				if (
					message.type === "hello-ok" &&
					"version" in message &&
					message.version === 1 &&
					"epoch" in message &&
					typeof message.epoch === "string"
				) {
					authenticated = true;
					backoff = 200;
					continue;
				}
				if (authenticated && (message.type === "pong" || message.type === "cancel")) continue;
				if (
					authenticated &&
					message.type === "snapshot" &&
					"requestId" in message &&
					typeof message.requestId === "string" &&
					message.requestId.length <= 128
				) {
					send({ type: "result", requestId: message.requestId, code: "APP_DIAGNOSTICS_UNSUPPORTED" });
					continue;
				}
				current.destroy();
				return;
			}
		});
		current.on("error", () => current.destroy());
		current.once("close", () => {
			clearInterval(heartbeat);
			if (disposed || terminal) return;
			retryTimer = setTimeout(open, backoff);
			backoff = Math.min(backoff * 2, 2_000);
		});
	};
	open();
	return () => {
		disposed = true;
		clearTimeout(retryTimer);
		clearInterval(heartbeat);
		socket?.destroy();
	};
}
