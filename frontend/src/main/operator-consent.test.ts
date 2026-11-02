// @vitest-environment node
import { describe, it, expect, vi } from "vitest";
import { createServer, type Socket } from "node:net";
import { setTimeout as realSleep } from "node:timers/promises";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { randomUUID } from "node:crypto";
import type { MessageBoxOptions } from "electron";
import { connectOperatorConsent, operatorConsentLabels, operatorConsentMenu } from "./operator-consent";

function fixedPlan() {
	return {
		protocol: "operator-consent/1", purpose: "settings.restore_permissions", domain: "settings", actionKey: "action-1", subjectRef: "subject-1",
		participantRefs: ["session-1"], scope: { projectRef: "project-1", ownerRef: "owner-1", branchRef: "branch-1", nativeRef: "native-1" },
		expectedRevisions: { owner: 1, permission: 2 }, payloadDigest: "a".repeat(64), effectDigest: "b".repeat(64), validUntil: new Date(Date.now() + 120000).toISOString(), riskCodes: ["PERMISSION_RESTORE"],
	};
}

// Protocol tests use explicit fake dialog results. These are not native gesture
// qualification or proof against XTEST / same-uid OS process control.
async function fixture(options: { plan?: object; plans?: readonly object[]; zh?: boolean; respond: (options: MessageBoxOptions) => number | Promise<number> }) {
	const directory = await mkdtemp(join(tmpdir(), "oc-"));
	const address = process.platform === "win32" ? `\\\\.\\pipe\\ao-oc-${randomUUID()}` : join(directory, "s");
	const frames: { type: string; requestId?: unknown; requestRef?: unknown; challenge?: unknown; digest?: unknown }[] = [];
	const sockets = new Set<Socket>();
	const plan = options.plan ?? fixedPlan();
	let reviewIndex = 0;
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
					case "review-next": send({ type: "review-plan", requestId, requestRef: `request-${++reviewIndex}`, challenge: "private-challenge", digest: "c".repeat(64), plan: options.plans?.[reviewIndex - 1] ?? plan }); break;
					case "confirm": send({ type: "confirmed", requestId, consentRef: "never-to-renderer" }); break;
					case "cancel": send({ type: "cancelled", requestId }); break;
				}
			}
		});
	});
	server.listen(address);
	await once(server, "listening");
	const show = vi.fn(async (dialog: MessageBoxOptions) => ({ response: await options.respond(dialog), checkboxChecked: false }));
	const link = connectOperatorConsent({ address, ownedToken: "private-owned-token", show, zh: () => options.zh ?? false });
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
			const allCalls = f.show.mock.calls.map(([options]) => options);
			const calls = allCalls.filter((options) => options.type === "question");
			expect(allCalls.at(-1)?.message).toBe("Confirmation recorded for this plan.");
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
	it.each([
		{ ...fixedPlan(), expectedRevisions: { permission: 2 ** 60 } },
		{ ...fixedPlan(), purpose: "coord.policy.update" },
		{ ...fixedPlan(), scope: null },
	])("keeps the issuer connected and reviews the next plan after rejecting an undisplayable plan", async (invalid) => {
		const next = { ...fixedPlan(), actionKey: "action-2" };
		const f = await fixture({ plans: [invalid, next], respond: () => 1 });
		try {
			await f.link.reviewNext();
			expect(f.show).toHaveBeenCalledOnce();
			expect(f.show.mock.calls[0][0].type).toBe("info");
			expect(f.frames.filter((frame) => frame.type === "cancel")).toEqual([expect.objectContaining({ requestRef: "request-1" })]);
			expect(f.frames.some((frame) => frame.type === "confirm")).toBe(false);
			await f.link.reviewNext();
			expect(f.frames.filter((frame) => frame.type === "operator-hello")).toHaveLength(1);
			expect(f.frames.filter((frame) => frame.type === "review-next")).toHaveLength(2);
			expect(f.show.mock.calls.filter(([options]) => options.type === "question").slice(0, -1).map(([options]) => options.detail).join("")).toBe(JSON.stringify(next, null, 2));
			expect(f.frames.filter((frame) => frame.type === "confirm")).toEqual([expect.objectContaining({ requestRef: "request-2" })]);
		} finally { await f.close(); }
	});
});

it("uses zh-TW labels and acknowledges recorded consent without claiming execution", async () => {
	expect(operatorConsentLabels(true)).toEqual({ menu: "操作者確認", review: "檢視下一個固定操作計畫…", notReady: "本機確認 issuer 尚未就緒。" });
	expect(operatorConsentLabels(false).review).toBe("Review next fixed action plan…");
	const f = await fixture({ zh: true, respond: () => 1 });
	try {
		await f.link.reviewNext();
		expect(f.show.mock.calls.at(-1)?.[0]).toMatchObject({ type: "info", message: "已記錄此計畫的確認。", detail: "這只記錄同意，不會自動執行操作。" });
	} finally { await f.close(); }
});

async function retryFixture(rejection?: string | ((attempt: number) => string)) {
	const directory = await mkdtemp(join(tmpdir(), "or-"));
	const address = process.platform === "win32" ? `\\\\.\\pipe\\ao-or-${randomUUID()}` : join(directory, "s");
	const sockets = new Set<Socket>();
	let attempts = 0;
	const server = createServer((socket) => {
		attempts++;
		sockets.add(socket);
		socket.once("close", () => sockets.delete(socket));
		socket.once("data", () => {
			const code = typeof rejection === "function" ? rejection(attempts) : rejection;
			if (code === "accept") socket.write(`${JSON.stringify({ type: "operator-hello-ok", version: 1 })}\n`);
			else if (code) socket.end(`${JSON.stringify({ type: "operator-hello-rejected", code })}\n`);
		});
	});
	server.listen(address);
	await once(server, "listening");
	const link = connectOperatorConsent({ address, show: async () => ({ response: 0, checkboxChecked: false }), zh: () => false });
	return { attempts: () => attempts, open: () => sockets.size, async close() {
		link.dispose();
		for (const socket of sockets) socket.destroy();
		await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
		await rm(directory, { recursive: true, force: true });
	} };
}

async function waitForIO(check: () => boolean) {
	for (let i = 0; i < 500 && !check(); i++) await realSleep(1);
	expect(check()).toBe(true);
	await realSleep(1);
}

it.each(["OPERATOR_PEER_UNQUALIFIED", "OPERATOR_TOKEN_INVALID", "OPERATOR_PLATFORM_UNVERIFIED", "OPERATOR_PROTOCOL_INVALID"])("stops retrying after permanent rejection %s", async (code) => {
	vi.useFakeTimers();
	const f = await retryFixture(code);
	try {
		await waitForIO(() => f.attempts() === 1);
		await waitForIO(() => f.open() === 0);
		await vi.advanceTimersByTimeAsync(3_600_000);
		expect(f.attempts()).toBe(1);
	} finally { await f.close(); vi.useRealTimers(); }
});

it.each([undefined, "OPERATOR_PIN_PENDING"])("bounds retries with exponential backoff for %s", async (code) => {
	vi.useFakeTimers();
	const f = await retryFixture(code);
	try {
		await waitForIO(() => f.attempts() === 1);
		for (const [index, delay] of [2_000, 4_000, 8_000, 16_000, 30_000].entries()) {
			if (code === undefined) await vi.advanceTimersByTimeAsync(5_000);
			await waitForIO(() => f.open() === 0);
			await vi.advanceTimersByTimeAsync(delay - 1);
			expect(f.attempts()).toBe(index + 1);
			await vi.advanceTimersByTimeAsync(1);
			await waitForIO(() => f.attempts() === index + 2);
		}
		if (code === undefined) await vi.advanceTimersByTimeAsync(5_000);
		await waitForIO(() => f.open() === 0);
		await vi.advanceTimersByTimeAsync(3_600_000);
		expect(f.attempts()).toBe(6);
	} finally { await f.close(); vi.useRealTimers(); }
});

it("keeps retrying BUSY beyond the failure limit with a capped backoff and can pair when the slot clears", async () => {
	vi.useFakeTimers();
	const f = await retryFixture((attempt) => attempt <= 8 ? "OPERATOR_ISSUER_BUSY" : "accept");
	try {
		await waitForIO(() => f.attempts() === 1);
		for (const [index, delay] of [2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000, 30_000].entries()) {
			await waitForIO(() => f.open() === 0);
			await vi.advanceTimersByTimeAsync(delay - 1);
			expect(f.attempts()).toBe(index + 1);
			await vi.advanceTimersByTimeAsync(1);
			await waitForIO(() => f.attempts() === index + 2);
		}
		await vi.advanceTimersByTimeAsync(60_000);
		expect(f.attempts()).toBe(9);
		expect(f.open()).toBe(1);
	} finally { await f.close(); vi.useRealTimers(); }
});


it("native Review has no accelerator and rejects accelerator-triggered clicks", () => {
	const review = vi.fn();
	const menu = operatorConsentMenu(true, review);
	expect(menu.label).toBe("操作者確認");
	for (const item of menu.submenu) {
		expect(item).not.toHaveProperty("accelerator");
		item.click(undefined, undefined, { triggeredByAccelerator: true });
		expect(review).not.toHaveBeenCalled();
		item.click(undefined, undefined, { triggeredByAccelerator: false });
	}
	expect(review).toHaveBeenCalledOnce();
});
