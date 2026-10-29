import fixture from "../../../test/fixtures/app-diagnostics-v1.json";
import { describe, expect, it, vi } from "vitest";
import { createServer } from "node:net";
import { once } from "node:events";
import { connectAppDiagnostics, privateRuntimeHandoff } from "./app-diagnostics-link";

describe("app diagnostics private link", () => {
	it("does not publish a stale disconnected state after disposal", async () => {
		const server = createServer();
		server.listen(0, "127.0.0.1");
		await once(server, "listening");
		const address = server.address();
		if (!address || typeof address === "string") throw new Error("missing server");
		const incoming = once(server, "connection");
		const state = vi.fn();
		const dispose = connectAppDiagnostics({ port: address.port, host: "127.0.0.1" }, "token", "app-1", { state });
		const [socket] = await incoming;
		try {
			await once(socket, "data");
			socket.write(JSON.stringify({ type: "hello-ok", version: 1, schemaVersion: 1, epoch: "epoch" }) + "\n");
			await vi.waitFor(() => expect(state).toHaveBeenCalledWith("connected"));
			state.mockClear();
			const closed = once(socket, "close");
			dispose();
			await closed;
			expect(state).not.toHaveBeenCalled();
		} finally {
			dispose();
			socket.destroy();
			server.close();
		}
	});
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
		socket.write(JSON.stringify({ type: "hello-ok", version: 1, schemaVersion: 1, epoch: "epoch-1" }) + "\n");
		socket.write(JSON.stringify({ type: "snapshot", schemaVersion: 1, requestId: "request-1", request: { windowId: "window-1" } }) + "\n");
		await new Promise<void>((resolve) => socket.once("data", () => resolve()));
		expect(frames[1]).toEqual({ type: "result", schemaVersion: 1, requestId: "request-1", code: "APP_DIAGNOSTICS_UNSUPPORTED" });
		expect(frames.some((frame) => typeof frame === "object" && frame !== null && "type" in frame && frame.type === "ready")).toBe(false);
		close();
		socket.destroy();
		server.close();
	});

	it.each(["project-1", "next.js", "socket.io"])("delivers a validated collector result for %s", async (projectId) => {
		const server = createServer();
		server.listen(0, "127.0.0.1");
		await once(server, "listening");
		const address = server.address();
		if (!address || typeof address === "string") throw new Error("missing server");
		const incoming = once(server, "connection");
		const capture = vi.fn(async () => fixture);
		const close = connectAppDiagnostics({ port: address.port, host: "127.0.0.1" }, "token", "app-1", {
			windows: () => [{ id: "window-1", generation: 1 }],
			capture,
		});
		const [socket] = await incoming;
		const frames: Array<{ type: string; snapshot?: unknown }> = [];
		let buffer = "";
		socket.on("data", (data: Buffer) => {
			buffer += data.toString();
			for (;;) {
				const end = buffer.indexOf("\n");
				if (end < 0) break;
				frames.push(JSON.parse(buffer.slice(0, end)));
				buffer = buffer.slice(end + 1);
			}
		});
		try {
			await vi.waitFor(() => expect(frames.some((frame) => frame.type === "hello")).toBe(true));
			socket.write(JSON.stringify({ type: "hello-ok", version: 1, schemaVersion: 1, epoch: "epoch" }) + "\n");
			await vi.waitFor(() => expect(frames.some((frame) => frame.type === "ready")).toBe(true));
			socket.write(
				JSON.stringify({ type: "snapshot", schemaVersion: 1, requestId: "request-1", request: { windowId: "window-1", projectId } }) + "\n",
			);
			await vi.waitFor(() => expect(frames.find((frame) => frame.type === "result")?.snapshot).toEqual(fixture));
			expect(capture).toHaveBeenCalledWith({ windowId: "window-1", projectId }, expect.any(AbortSignal));
		} finally {
			close();
			socket.destroy();
			server.close();
		}
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
						if (reason === "disconnect")
							socket.end(`${JSON.stringify({ type: "hello-ok", version: 1, schemaVersion: 1, epoch: "epoch" })}\n`);
						else socket.end(JSON.stringify({ type: "hello-rejected", code: reason }) + "\n");
					} else {
						socket.write(JSON.stringify({ type: "hello-ok", version: 1, schemaVersion: 1, epoch: "epoch" }) + "\n");
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
});

it("correlates native grant/revoke without exposing credentials", async () => {
	const server = createServer();
	server.listen(0, "127.0.0.1");
	await once(server, "listening");
	const address = server.address();
	if (!address || typeof address === "string") throw new Error("missing address");
	const incoming = once(server, "connection");
	const state = vi.fn();
	const link = connectAppDiagnostics({ port: address.port, host: "127.0.0.1" }, "private-token", "app-1", { state });
	const [socket] = await incoming;
	let buffer = "";
	const frames: Array<{ type: string; requestId?: string; sessionId?: string; grantId?: string }> = [];
	socket.on("data", (chunk: Buffer) => {
		buffer += chunk.toString();
		for (;;) {
			const end = buffer.indexOf("\n");
			if (end < 0) break;
			frames.push(JSON.parse(buffer.slice(0, end)));
			buffer = buffer.slice(end + 1);
		}
	});
	try {
		await vi.waitFor(() => expect(frames[0]?.type).toBe("hello"));
		socket.write(JSON.stringify({ type: "hello-ok", version: 1, schemaVersion: 1, epoch: "epoch" }) + "\n");
		await vi.waitFor(() => expect(state).toHaveBeenCalledWith("connected"));
		const granting = link.grant("next.js-1");
		await vi.waitFor(() => expect(frames.at(-1)?.type).toBe("grant"));
		expect(frames.at(-1)?.sessionId).toBe("next.js-1");
		expect(Reflect.get(frames.at(-1)!, "schemaVersion")).toBe(1);
		socket.write(
			JSON.stringify({ type: "grant-result", schemaVersion: 1, requestId: frames.at(-1)?.requestId, grantId: "grant-1" }) + "\n",
		);
		expect(await granting).toBe("grant-1");
		const revoking = link.revoke("grant-1");
		await vi.waitFor(() => expect(frames.at(-1)?.type).toBe("revoke"));
		socket.write(JSON.stringify({ type: "revoke-result", schemaVersion: 1, requestId: frames.at(-1)?.requestId }) + "\n");
		await revoking;
	} finally {
		link();
		socket.destroy();
		server.close();
	}
});

it("marks an older daemon unsupported before sending new snapshot frames", async () => {
	const server = createServer();
	server.listen(0, "127.0.0.1");
	await once(server, "listening");
	const address = server.address();
	if (!address || typeof address === "string") throw new Error("missing address");
	const incoming = once(server, "connection");
	const state = vi.fn();
	const link = connectAppDiagnostics({ port: address.port, host: "127.0.0.1" }, "token", "app-1", { state });
	const [socket] = await incoming;
	try {
		await once(socket, "data");
		socket.write(JSON.stringify({ type: "hello-ok", version: 1, epoch: "epoch" }) + "\n");
		await vi.waitFor(() => expect(state).toHaveBeenCalledWith("unsupported"));
		await expect(link.grant("next.js-1")).rejects.toThrow("APP_NOT_CONNECTED");
	} finally {
		link();
		socket.destroy();
		server.close();
	}
});
