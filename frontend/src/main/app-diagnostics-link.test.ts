import { describe, expect, it } from "vitest";
import { createServer } from "node:net";
import { once } from "node:events";
import { connectAppDiagnostics, privateRuntimeHandoff } from "./app-diagnostics-link";

describe("app diagnostics private link", () => {
	it("hands independent credentials to the daemon without exporting them", () => {
		const [legacy, extra] = privateRuntimeHandoff("browser-token", "app-token").trim().split("\n");
		expect(legacy).toBe("browser-token");
		expect(JSON.parse(extra)).toEqual({ version: 1, browserToken: "browser-token", appDiagnosticsToken: "app-token" });
	});
	it("authenticates separately and never advertises a missing collector", async () => {
		const server = createServer();
		server.listen(0, "127.0.0.1");
		await once(server, "listening");
		const address = server.address();
		if (!address || typeof address === "string") throw new Error("missing server");
		const frames: unknown[] = [];
		const accepted = once(server, "connection");
		const close = connectAppDiagnostics({ port: address.port, host: "127.0.0.1" }, "app-token", "app-instance");
		const [socket] = await accepted;
		let pending = "";
		socket.on("data", (chunk: Buffer) => {
			pending += chunk.toString();
			for (;;) {
				const end = pending.indexOf("\n");
				if (end < 0) break;
				frames.push(JSON.parse(pending.slice(0, end)));
				pending = pending.slice(end + 1);
			}
		});
		await new Promise<void>((resolve) => socket.once("data", () => resolve()));
		expect(frames).toEqual([{ type: "hello", version: 1, token: "app-token", instanceId: "app-instance" }]);
		socket.write(JSON.stringify({ type: "hello-ok", version: 1, epoch: "epoch-1" }) + "\n");
		socket.write(JSON.stringify({ type: "snapshot", requestId: "request-1", request: { windowId: "window-1" } }) + "\n");
		await new Promise<void>((resolve) => socket.once("data", () => resolve()));
		expect(frames[1]).toEqual({ type: "result", requestId: "request-1", code: "APP_DIAGNOSTICS_UNSUPPORTED" });
		expect(frames.some((frame) => typeof frame === "object" && frame !== null && "type" in frame && frame.type === "ready")).toBe(false);
		close();
		socket.destroy();
		server.close();
	});
});

it.each(["disconnect", "APP_DIAGNOSTICS_BUSY"])("reconnects after %s", async (reason) => {
	const server = createServer();
	server.listen(0, "127.0.0.1");
	await once(server, "listening");
	const address = server.address();
	if (!address || typeof address === "string") throw new Error("missing server");
	let attempts = 0;
	const reconnected = new Promise<void>((resolve) =>
		server.on("connection", (socket) => {
			attempts++;
			socket.on("error", (error) => {
				if (!("code" in error) || error.code !== "ECONNRESET") throw error;
			});
			socket.once("data", () => {
				if (attempts === 1) {
					if (reason === "disconnect") socket.end(`${JSON.stringify({ type: "hello-ok", version: 1, epoch: "epoch" })}\n`);
					else socket.end(JSON.stringify({ type: "hello-rejected", code: reason }) + "\n");
				} else {
					socket.write(JSON.stringify({ type: "hello-ok", version: 1, epoch: "epoch" }) + "\n");
					resolve();
				}
			});
		}),
	);
	const close = connectAppDiagnostics({ port: address.port, host: "127.0.0.1" }, "token", "instance");
	let timer: ReturnType<typeof setTimeout> | undefined;
	try {
		await Promise.race([
			reconnected,
			new Promise((_, reject) => {
				timer = setTimeout(() => reject(new Error("no reconnect")), 1500);
			}),
		]);
	} finally {
		clearTimeout(timer);
		close();
		server.close();
	}
});

it("does not retry a rejected capability", async () => {
	const server = createServer();
	server.listen(0, "127.0.0.1");
	await once(server, "listening");
	const address = server.address();
	if (!address || typeof address === "string") throw new Error("missing server");
	let attempts = 0;
	server.on("connection", (socket) => {
		attempts++;
		socket.on("error", (error) => {
			if (!("code" in error) || error.code !== "ECONNRESET") throw error;
		});
		socket.once("data", () => socket.end(JSON.stringify({ type: "hello-rejected", code: "APP_DIAGNOSTICS_CAPABILITY_INVALID" }) + "\n"));
	});
	const close = connectAppDiagnostics({ port: address.port, host: "127.0.0.1" }, "wrong", "instance");
	try {
		await new Promise((resolve) => setTimeout(resolve, 700));
		expect(attempts).toBe(1);
	} finally {
		close();
		server.close();
	}
});
