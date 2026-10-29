import { describe, expect, it, vi } from "vitest";
import fixture from "../../../test/fixtures/app-diagnostics-v1.json";
import { isAppSnapshot, isCollector } from "./app-diagnostics";

describe("app diagnostic three-boundary allowlist", () => {
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
