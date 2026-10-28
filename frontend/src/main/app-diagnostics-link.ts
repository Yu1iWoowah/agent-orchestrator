import { connect, type NetConnectOpts } from "node:net";

/** Keep the legacy first line so an older daemon still authenticates Browser. */
export function privateRuntimeHandoff(browserToken: string, appDiagnosticsToken: string): string {
	return `${browserToken}\n${JSON.stringify({ version: 1, browserToken, appDiagnosticsToken })}\n`;
}

/** PR1 has no collector or consent UI. It never advertises ready or issues grants. */
export function connectAppDiagnostics(address: NetConnectOpts, token: string, instanceId: string): () => void {
	const socket = connect(address);
	let buffer = "";
	let authenticated = false;
	const send = (value: unknown) => {
		if (!socket.destroyed) socket.write(`${JSON.stringify(value)}\n`);
	};
	const timer = setInterval(() => {
		if (authenticated) send({ type: "ping" });
	}, 5_000);
	socket.once("connect", () => send({ type: "hello", version: 1, token, instanceId }));
	socket.on("data", (chunk: Buffer) => {
		buffer += chunk.toString("utf8");
		if (Buffer.byteLength(buffer) > 1 << 20) {
			socket.destroy();
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
				socket.destroy();
				return;
			}
			if (typeof message !== "object" || message === null || !("type" in message)) {
				socket.destroy();
				return;
			}
			if (message.type === "hello-ok" && "version" in message && message.version === 1 && "epoch" in message && typeof message.epoch === "string") {
				authenticated = true;
				continue;
			}
			if (authenticated && (message.type === "pong" || message.type === "cancel")) continue;
			if (authenticated && message.type === "snapshot" && "requestId" in message && typeof message.requestId === "string" && message.requestId.length <= 128) {
				send({ type: "result", requestId: message.requestId, code: "APP_DIAGNOSTICS_UNSUPPORTED" });
				continue;
			}
			socket.destroy();
			return;
		}
	});
	socket.on("error", () => socket.destroy());
	socket.once("close", () => clearInterval(timer));
	return () => {
		clearInterval(timer);
		socket.destroy();
	};
}
