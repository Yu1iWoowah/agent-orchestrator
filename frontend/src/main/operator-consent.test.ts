// @vitest-environment node
import { describe, it, expect, vi } from "vitest";
import { createServer, type Socket } from "node:net";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { randomUUID } from "node:crypto";
import type { MessageBoxOptions } from "electron";
import { connectOperatorConsent } from "./operator-consent";

function fixedPlan() {
	return {
		protocol: "operator-consent/1", purpose: "settings.restore_permissions", domain: "settings", actionKey: "action-1", subjectRef: "subject-1",
		participantRefs: ["session-1"], scope: { projectRef: "project-1", ownerRef: "owner-1", branchRef: "branch-1", nativeRef: "native-1" },
		expectedRevisions: { owner: 1, permission: 2 }, payloadDigest: "a".repeat(64), effectDigest: "b".repeat(64), validUntil: new Date(Date.now() + 120000).toISOString(), riskCodes: ["PERMISSION_RESTORE"],
	};
}

// Protocol tests use explicit fake dialog results. These are not native gesture
// qualification or proof against XTEST / same-uid OS process control.
async function fixture(options: { plan?: object; respond: (options: MessageBoxOptions) => number | Promise<number> }) {
	const directory = await mkdtemp(join(tmpdir(), "oc-"));
	const address = process.platform === "win32" ? `\\\\.\\pipe\\ao-oc-${randomUUID()}` : join(directory, "s");
	const frames: { type: string; requestId?: unknown; requestRef?: unknown; challenge?: unknown; digest?: unknown }[] = [];
	const sockets = new Set<Socket>();
	const plan = options.plan ?? fixedPlan();
	const server = createServer((socket) => {
		sockets.add(socket);
		socket.on("close", () => sockets.delete(socket));
		let buffer = "";
		socket.on("data", (chunk: Buffer) => {
			buffer += chunk.toString("utf8");
			for (;;) {
				const end = buffer.indexOf("\n");
				if (end < 0) break;
				const value: unknown = JSON.parse(buffer.slice(0, end));
				buffer = buffer.slice(end + 1);
				if (typeof value !== "object" || value === null || !("type" in value) || typeof value.type !== "string") throw new Error("bad test frame");
				frames.push({ ...value, type: value.type });
				const requestId = "requestId" in value ? value.requestId : undefined;
				const send = (reply: object) => socket.write(`${JSON.stringify(reply)}\n`);
				switch (value.type) {
					case "operator-hello": send({ type: "operator-hello-ok", version: 1 }); break;
					case "ready": case "ping": send({ type: "pong" }); break;
					case "review-next": send({ type: "review-plan", requestId, requestRef: "request-1", challenge: "private-challenge", digest: "c".repeat(64), plan }); break;
					case "confirm": send({ type: "confirmed", requestId, consentRef: "never-to-renderer" }); break;
					case "cancel": send({ type: "cancelled", requestId }); break;
				}
			}
		});
	});
	server.listen(address);
	await once(server, "listening");
	const show = vi.fn(async (dialog: MessageBoxOptions) => ({ response: await options.respond(dialog), checkboxChecked: false }));
	const link = connectOperatorConsent({ address, ownedToken: "private-owned-token", show, zh: () => false });
	await vi.waitFor(() => expect(frames.some((frame) => frame.type === "ready")).toBe(true));
	return { frames, show, link, plan, async close() {
		link.dispose();
		for (const socket of sockets) socket.destroy();
		await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
		await rm(directory, { recursive: true, force: true });
	} };
}

describe("private operator confirmation UI", () => {
	it("disposal aborts a pending native dialog without confirming or reopening a dialog", async () => {
		const f = await fixture({ respond: (options) => new Promise<number>((resolve) => options.signal?.addEventListener("abort", () => resolve(0), { once: true })) });
		try {
			const reviewing = f.link.reviewNext();
			await vi.waitFor(() => expect(f.show).toHaveBeenCalledOnce());
			f.link.dispose();
			await reviewing;
			expect(f.frames.some((frame) => frame.type === "confirm")).toBe(false);
			expect(f.show).toHaveBeenCalledOnce();
		} finally { await f.close(); }
	});
	it("does not open on prepare/connection and cancelling never confirms", async () => {
		const f = await fixture({ respond: () => 0 });
		try {
			expect(f.show).not.toHaveBeenCalled();
			await f.link.reviewNext();
			expect(f.frames.some((frame) => frame.type === "confirm")).toBe(false);
			expect(f.frames.some((frame) => frame.type === "cancel")).toBe(true);
			expect(f.show.mock.calls[0][0]).toMatchObject({ defaultId: 0, cancelId: 0, noLink: true });
		} finally { await f.close(); }
	});
	it("displays the entire fixed batch and sends only its bound confirmation", async () => {
		const plan = { ...fixedPlan(), participantRefs: Array.from({ length: 64 }, (_, i) => `session-${i}`) };
		const f = await fixture({ plan, respond: () => 1 });
		try {
			const result = await f.link.reviewNext();
			expect(result).toBeUndefined();
			const calls = f.show.mock.calls.map(([options]) => options);
			expect(calls.slice(0, -1).map((options) => options.detail).join("")).toBe(JSON.stringify(plan, null, 2));
			expect(calls.length).toBeGreaterThan(3);
			expect(calls.slice(0, -1).every((options) => (options.detail?.length ?? 0) <= 1000 && (options.detail?.match(/\n/g)?.length ?? 0) <= 12)).toBe(true);
			expect(calls.at(-1)?.detail).toContain("same-account exec/file access, XTEST or OS injection");
			expect(calls.every((options) => options.defaultId === 0 && options.cancelId === 0)).toBe(true);
			expect(f.frames.filter((frame) => frame.type === "confirm")).toEqual([expect.objectContaining({ requestRef: "request-1", challenge: "private-challenge", digest: "c".repeat(64) })]);
			expect(JSON.stringify(calls)).not.toContain("private-owned-token");
			expect(JSON.stringify(calls)).not.toContain("private-challenge");
			expect(JSON.stringify(calls)).not.toContain("never-to-renderer");
		} finally { await f.close(); }
	});
	it.each([
		{ ...fixedPlan(), purpose: "coord.policy.update" },
		{ ...fixedPlan(), scope: { ...fixedPlan().scope, ownerRef: "forged\nbody" } },
		{ ...fixedPlan(), prompt: "untrusted extra content" },
		{ ...fixedPlan(), expectedRevisions: { permission: 2 ** 60 } },
	])("rejects unqualified or malformed native plans", async (plan) => {
		const f = await fixture({ plan, respond: () => 1 });
		try {
			await f.link.reviewNext();
			expect(f.frames.some((frame) => frame.type === "confirm")).toBe(false);
			expect(f.show.mock.calls.every(([options]) => options.type === "info")).toBe(true);
		} finally { await f.close(); }
	});
});
