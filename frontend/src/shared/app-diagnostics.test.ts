import { describe, expect, it, vi } from "vitest";
import fixture from "../../../test/fixtures/app-diagnostics-v1.json";
import { diagnosticId, isAppSnapshot, isCollector, isCollectorRequest, isCollectorReply } from "./app-diagnostics";

describe("app diagnostic three-boundary allowlist", () => {
	it("requires schemaVersion 1 for requests and success/failure replies", () => {
		const identity = { requestId: "request-1", documentId: "document-1", windowId: "window-1", generation: 1 };
		for (const [guard, value] of [
			[isCollectorRequest, { ...identity, projectId: "next.js", deadline: Date.now() + 5000 }],
			[isCollectorReply, { ...identity, collector: fixture.collector }],
			[isCollectorReply, { ...identity, code: "APP_RENDERER_UNAVAILABLE" }],
		] as const) {
			expect(guard(value)).toBe(false);
			expect(guard({ ...value, schemaVersion: 2 })).toBe(false);
			expect(guard({ ...value, schemaVersion: 1 })).toBe(true);
		}
	});
	it.each(["next.js", "socket.io"])("accepts dotted project/session ids for %s", (project) => {
		const snapshot = JSON.parse(JSON.stringify(fixture).replaceAll("project-1", project).replaceAll("worker-1", `${project}-1`));
		expect(isAppSnapshot(snapshot)).toBe(true);
	});
	it.each(["/private/path", "next.js/../secret", "next\\js"])("still rejects path separators in %s", (id) =>
		expect(diagnosticId(id)).toBe(false),
	);
	it("reports bounded ancestor chains separately from missing nodes", () => {
		const snapshot = structuredClone(fixture);
		snapshot.completeness = "complete";
		snapshot.collector.geometry.availability = "available";
		Object.assign(snapshot.collector.geometry, { ancestorTruncatedCount: 1 });
		expect(isAppSnapshot(snapshot)).toBe(true);
	});
	it("accepts the same versioned fixture as the daemon", () => {
		expect(isCollector(fixture.collector)).toBe(true);
		expect(isAppSnapshot(fixture)).toBe(true);
	});
	it.each(["payload", "token", "path", "message", "props"])("rejects unknown %s in every nested section", (key) => {
		for (const section of ["route", "shell", "cache", "components", "geometry", "errors"] as const) {
			const value = structuredClone(fixture.collector);
			Object.assign(value[section], { [key]: "canary-secret /private/path" });
			expect(isCollector(value)).toBe(false);
		}
	});
	it("rejects arbitrary error content, future schemas and oversize data", () => {
		const value = structuredClone(fixture.collector);
		Object.assign(value.errors, {
			records: [{ at: fixture.capturedAt, source: "console_error", name: "Error", message: "canary-secret", count: 1 }],
		});
		expect(isCollector(value)).toBe(false);
		expect(isCollector({ ...fixture.collector, redactionSchemaVersion: 2 })).toBe(false);
		expect(isCollector({ ...fixture.collector, payload: "s".repeat(1 << 20) })).toBe(false);
	});
	it("rejects accessors without executing them at the IPC boundary", () => {
		const value = structuredClone(fixture.collector);
		const getter = vi.fn(() => "project-1");
		Object.defineProperty(value.route, "projectId", { get: getter, enumerable: true });
		expect(isCollector(value)).toBe(false);
		expect(getter).not.toHaveBeenCalled();
	});
});
