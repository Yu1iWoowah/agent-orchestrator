import { afterEach, describe, expect, it, vi } from "vitest";
import fixture from "../../../test/fixtures/app-diagnostics-v1.json";
import { AppDiagnosticsMain, type CollectorRequest, type DiagnosticsShell } from "./app-diagnostics";
import { DIAGNOSTIC_CHANNEL, isAppSnapshot } from "../shared/app-diagnostics";
const all: AppDiagnosticsMain[] = [];
function harness() {
	const frame = { url: "app://renderer/index.html" };
	const send = vi.fn();
	let shell: DiagnosticsShell | null = { id: 1, mainFrame: frame, isDestroyed: () => false, send };
	const main = new AppDiagnosticsMain({
		instanceId: "app-1",
		rendererURL: () => frame.url,
		shell: () => shell,
		identity: () => fixture.identity,
	});
	all.push(main);
	const event = { sender: shell, senderFrame: frame };
	main.setConnection({ connected: true });
	return {
		main,
		event,
		send,
		frame,
		removeWindow: () => {
			shell = null;
		},
	};
}
function request(send: ReturnType<typeof vi.fn>): CollectorRequest {
	const call = send.mock.calls.findLast(([channel]) => channel === DIAGNOSTIC_CHANNEL.request);
	if (!call) throw new Error("renderer did not receive request");
	return call[1];
}
function reply(incoming: CollectorRequest) {
	return {
		schemaVersion: 1,
		requestId: incoming.requestId,
		documentId: incoming.documentId,
		windowId: incoming.windowId,
		generation: incoming.generation,
		collector: fixture.collector,
	};
}
afterEach(() => {
	for (const main of all.splice(0)) main.dispose();
	vi.useRealTimers();
});
describe("trusted app collector broker", () => {
	it.each([undefined, 2])("rejects correlated replies with schemaVersion %s", async (schemaVersion) => {
		const { main, event, send } = harness();
		main.allowUntil(Date.now() + 900000);
		main.register(event);
		const pending = main.capture({}, new AbortController().signal);
		const rejected = expect(pending).rejects.toThrow("APP_DIAGNOSTICS_INVALID_PAYLOAD");
		const incoming = request(send);
		const accepted = main.submit(event, { ...reply(incoming), schemaVersion });
		await rejected;
		expect(Reflect.get(incoming, "schemaVersion")).toBe(1);
		expect(accepted).toBe(false);
	});
	it("reports a fixed renderer failure immediately without exporting its content", async () => {
		const { main, event, send } = harness();
		main.allowUntil(Date.now() + 900_000);
		main.register(event);
		const pending = main.capture({}, new AbortController().signal);
		const rejected = expect(pending).rejects.toThrow("APP_DIAGNOSTICS_PAYLOAD_TOO_LARGE");
		const incoming = request(send);
		const accepted = main.submit(event, {
			schemaVersion: 1,
			requestId: incoming.requestId,
			documentId: incoming.documentId,
			windowId: incoming.windowId,
			generation: incoming.generation,
			code: "APP_DIAGNOSTICS_PAYLOAD_TOO_LARGE",
		});
		await rejected;
		expect(accepted).toBe(true);
	});
	it("reports Q15-A and cannot repair adoption by consenting again", () => {
		const { main, send } = harness();
		main.setConnection({ connected: false, adopted: true });
		expect(main.status()).toEqual({ state: "unavailable", reason: "adopted_daemon" });
		expect(() => main.allowUntil(Date.now() + 900_000)).toThrow();
		expect(main.windows()).toEqual([]);
		expect(send.mock.calls.some(([, value]) => value?.token)).toBe(false);
	});
	it("requires consent and the live main window", async () => {
		const { main, removeWindow, event } = harness();
		main.register(event);
		expect(main.windows()).toEqual([]);
		await expect(main.capture({}, new AbortController().signal)).rejects.toThrow("APP_DIAGNOSTICS_DISABLED");
		main.allowUntil(Date.now() + 900_000);
		removeWindow();
		expect(main.status().reason).toBe("no_window");
		await expect(main.capture({}, new AbortController().signal)).rejects.toThrow("APP_WINDOW_GONE");
	});
	it("only accepts the current top frame/document/request/window once", async () => {
		const { main, event, send } = harness();
		main.allowUntil(Date.now() + 900_000);
		main.register(event);
		const pending = main.capture({ windowId: "window-1", projectId: "project-1" }, new AbortController().signal);
		const incoming = request(send);
		expect(() => main.submit({ sender: {}, senderFrame: event.senderFrame }, reply(incoming))).toThrow("APP_DIAGNOSTICS_UNTRUSTED_FRAME");
		expect(() => main.submit({ ...event, senderFrame: { url: "app://renderer/index.html" } }, reply(incoming))).toThrow(
			"APP_DIAGNOSTICS_UNTRUSTED_FRAME",
		);
		expect(main.submit(event, { ...reply(incoming), documentId: "old-document" })).toBe(false);
		expect(main.submit(event, { ...reply(incoming), windowId: "another-window" })).toBe(false);
		expect(main.submit(event, reply(incoming))).toBe(true);
		const result = await pending;
		expect(isAppSnapshot(result)).toBe(true);
		expect(result.collector).toEqual(fixture.collector);
		expect(main.submit(event, reply(incoming))).toBe(false);
	});
	it.each(["unknown", "oversize"])("rejects %s payload without leaking it", async (kind) => {
		const { main, event, send } = harness();
		main.allowUntil(Date.now() + 900_000);
		main.register(event);
		const pending = main.capture({}, new AbortController().signal);
		const rejected = expect(pending).rejects.toThrow("APP_DIAGNOSTICS_INVALID_PAYLOAD");
		main.submit(event, { ...reply(request(send)), payload: "canary-secret".repeat(kind === "oversize" ? 100000 : 1) });
		await rejected;
	});
	it.each(["APP_RENDERER_CHANGED", "APP_RENDERER_UNAVAILABLE"])(
		"cancels on reload/crash %s without accepting a late reply",
		async (code) => {
			const { main, event, send } = harness();
			main.allowUntil(Date.now() + 900_000);
			main.register(event);
			const pending = main.capture({}, new AbortController().signal);
			const incoming = request(send);
			const rejected = expect(pending).rejects.toThrow(code);
			main.resetDocument(code);
			await rejected;
			expect(main.submit(event, reply(incoming))).toBe(false);
			const document = main.register(event);
			expect(document.generation).toBeGreaterThan(incoming.generation);
			expect(document.enabled).toBe(true); // Reload preserves only the unextended consent deadline.
		},
	);
	it("expires pending captures and consent without replacing evidence", async () => {
		vi.useFakeTimers();
		const { main, event, send } = harness();
		main.allowUntil(Date.now() + 900_000);
		main.register(event);
		const pending = main.capture({}, new AbortController().signal);
		const incoming = request(send);
		const rejected = expect(pending).rejects.toThrow("APP_SNAPSHOT_TIMEOUT");
		await vi.advanceTimersByTimeAsync(5000);
		await rejected;
		expect(main.submit(event, reply(incoming))).toBe(false);
		await vi.advanceTimersByTimeAsync(895000);
		expect(main.windows()).toEqual([]);
	});
	it("cancels immediately on revoke or HTTP abort", async () => {
		const { main, event, send } = harness();
		main.allowUntil(Date.now() + 900_000);
		main.register(event);
		const abort = new AbortController();
		const pending = main.capture({}, abort.signal);
		const incoming = request(send);
		const rejected = expect(pending).rejects.toThrow();
		abort.abort();
		await rejected;
		expect(main.submit(event, reply(incoming))).toBe(false);
		main.allowUntil(0);
		expect(main.windows()).toEqual([]);
	});
});
