import { act, cleanup, render } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useRef } from "react";
import { createAppDiagnosticsCollector } from "./app-diagnostics";
import { diagnosticRegions, setDiagnosticRecording, useDiagnosticCommit } from "./app-diagnostics-registry";
import { isCollector } from "../../shared/app-diagnostics";

const empty = () => ({
	kind: "project",
	omittedCount: 0,
	projectId: "project-1",
	hostId: "",
	source: "local",
	expanded: true,
	shared: false,
	showAll: false,
	sessionCount: 1,
	hiddenCount: 0,
	liveIds: ["worker-1"],
	workerIds: ["worker-1"],
	orderedIds: ["worker-1"],
	listedIds: ["worker-1"],
	workspaces: [],
});
function CommittedProject({
	stale,
	absent,
	hidden,
	projectId = "project-1",
	sessionId = "worker-1",
}: {
	stale?: boolean;
	absent?: boolean;
	hidden?: boolean;
	projectId?: string;
	sessionId?: string;
}) {
	const root = useRef<HTMLDivElement>(null);
	const list = useRef<HTMLDivElement>(null);
	useDiagnosticCommit(
		"project",
		() => ({
			...empty(),
			projectId,
			liveIds: [sessionId],
			orderedIds: [sessionId],
			listedIds: stale ? [] : [sessionId],
			workerIds: stale ? [] : [sessionId],
		}),
		() => root.current,
		() => list.current,
	);
	return (
		<div ref={root} style={{ visibility: hidden ? "hidden" : "visible" }} title="canary-secret">
			<div ref={list}>
				{!absent && (
					<div data-session-row="" aria-label="canary-secret">
						canary-secret
					</div>
				)}
			</div>
			<input defaultValue="canary-secret" />
		</div>
	);
}
function setup() {
	const queryClient = new QueryClient();
	queryClient.setQueryData(
		["workspaces"],
		[
			{
				id: "project-1",
				path: "/private/canary-secret",
				name: "canary-secret",
				sessions: [
					{
						id: "worker-1",
						kind: "worker",
						status: "working",
						activity: { state: "active" },
						title: "canary-secret",
						artifactFiles: [{ path: "/private/canary-secret" }],
					},
				],
			},
		],
	);
	queryClient.setQueryData(["conversation", "canary-secret"], { token: "canary-secret" });
	queryClient.setQueryData(["cloud-sessions"], [{ id: "canary-secret", token: "canary-secret" }]);
	const collector = createAppDiagnosticsCollector({
		queryClient,
		readRoute: () => ({ routeId: "board", projectId: "project-1", sessionId: "", hostId: "", source: "local", revision: 1 }),
	});
	return { queryClient, collector };
}
afterEach(() => {
	cleanup();
	setDiagnosticRecording(false);
	vi.restoreAllMocks();
});
describe("content-free committed app collector", () => {
	it.each(["next.js", "socket.io"])("collects dotted project %s without dropping cache, commit or row geometry", (projectId) => {
		const sessionId = `${projectId}-1`;
		const queryClient = new QueryClient();
		queryClient.setQueryData(["workspaces"], [{ id: projectId, sessions: [{ id: sessionId, kind: "worker" }] }]);
		const collector = createAppDiagnosticsCollector({
			queryClient,
			readRoute: () => ({ routeId: "board", projectId, sessionId: "", hostId: "", source: "local", revision: 1 }),
		});
		render(<CommittedProject projectId={projectId} sessionId={sessionId} />);
		act(() => collector.enable());
		try {
			const result = collector.capture(projectId);
			expect(result.cache.queries[0].workspaces[0].sessions[0].id).toBe(sessionId);
			expect(result.components.entries[0].listedIds).toEqual([sessionId]);
			expect(result.geometry.nodes.find((node) => node.role === "session_row")).toMatchObject({
				projectId,
				sessionId,
				availability: "available",
			});
		} finally {
			act(() => collector.disable());
		}
	});
	it.each(["stale-props", "missing-dom", "hidden-dom"])("keeps cache/commit/DOM evidence separate for %s", (kind) => {
		const { collector } = setup();
		render(<CommittedProject stale={kind === "stale-props"} absent={kind === "missing-dom"} hidden={kind === "hidden-dom"} />);
		act(() => collector.enable());
		const snapshot = collector.capture("project-1");
		expect(isCollector(snapshot)).toBe(true);
		expect(snapshot.cache.queries.find((q) => q.kind === "workspaces")?.workspaces[0].sessions[0].id).toBe("worker-1");
		const project = snapshot.components.entries.find((entry) => entry.kind === "project");
		expect(project?.listedIds).toEqual(kind === "stale-props" ? [] : ["worker-1"]);
		if (kind === "missing-dom")
			expect(snapshot.geometry.nodes.some((node) => node.role === "session_row" && node.availability === "not_mounted")).toBe(true);
		if (kind === "hidden-dom")
			expect(snapshot.geometry.nodes.some((node) => node.ancestors.some((ancestor) => ancestor.visibility === "hidden"))).toBe(true);
		expect(JSON.stringify(snapshot)).not.toContain("canary-secret");
		collector.disable();
	});
	it("does not fetch, mutate cache/storage or route while capturing; clears recording", () => {
		const { queryClient, collector } = setup();
		render(<CommittedProject />);
		act(() => collector.enable());
		const fetch = vi.spyOn(queryClient, "fetchQuery");
		const mutate = vi.spyOn(queryClient, "setQueryData");
		const invalidate = vi.spyOn(queryClient, "invalidateQueries");
		const storage = vi.spyOn(Storage.prototype, "setItem");
		const before = queryClient.getQueryData(["workspaces"]);
		collector.capture("project-1");
		expect(fetch).not.toHaveBeenCalled();
		expect(mutate).not.toHaveBeenCalled();
		expect(invalidate).not.toHaveBeenCalled();
		expect(storage).not.toHaveBeenCalled();
		expect(queryClient.getQueryData(["workspaces"])).toBe(before);
		act(() => collector.disable());
		expect(diagnosticRegions().regions).toEqual([]);
		expect(() => collector.capture("project-1")).toThrow();
	});
	it("records redacted errors only after enable, restores hooks and bounds the ring", () => {
		const original = vi.spyOn(console, "error").mockImplementation(() => undefined);
		const { collector } = setup();
		collector.enable();
		for (let i = 0; i < 150; i++) console.error(new TypeError(`canary-secret ${i}`));
		const value = collector.capture("project-1");
		expect(value.errors.records.length).toBeLessThanOrEqual(100);
		expect(value.errors.records[0]?.count).toBe(150);
		expect(value.errors.notRecordedBeforeEnable).toBe(true);
		expect(JSON.stringify(value)).not.toContain("canary-secret");
		collector.disable();
		expect(console.error).toBe(original);
	});
});

describe("collector consistency and bounds", () => {
	it("does not record through retained wrappers while disabled or double-count after re-enable", () => {
		const original = vi.spyOn(console, "error").mockImplementation(() => undefined);
		const { collector } = setup();
		collector.enable();
		const previous = console.error;
		const outer: typeof console.error = (...args) => previous(...args);
		console.error = outer;
		collector.disable();
		try {
			console.error(new RangeError("disabled-secret"));
			collector.enable();
			console.error(new TypeError("enabled-secret"));
			const result = collector.capture("project-1");
			expect(result.errors.records.map((row) => [row.name, row.count])).toEqual([["TypeError", 1]]);
		} finally {
			collector.disable();
			console.error = original;
		}
	});
	it("never evaluates error getters and makes ring overflow observable", () => {
		const original = vi.spyOn(console, "error").mockImplementation(() => undefined);
		const { collector } = setup();
		collector.enable();
		const getter = vi.fn(() => {
			throw new Error("canary-secret");
		});
		const error = new TypeError("canary-secret");
		Object.defineProperty(error, "name", { get: getter });
		try {
			expect(() => console.error(error, { token: "canary-secret" })).not.toThrow();
			for (let i = 0; i < 150; i++) console.error(i % 2 ? new TypeError("canary-secret") : new Error("canary-secret"));
			const result = collector.capture("project-1");
			expect(getter).not.toHaveBeenCalled();
			expect(result.errors.records).toHaveLength(100);
			expect(result.errors.omittedCount).toBeGreaterThan(0);
			expect(JSON.stringify(result)).not.toContain("canary-secret");
			expect(original).toHaveBeenCalled();
		} finally {
			collector.disable();
		}
	});
	it("reports bounded ancestor chains separately from missing nodes", () => {
		const { collector } = setup();
		render(
			<div>
				<div>
					<div>
						<div>
							<div>
								<div>
									<div>
										<div>
											<div>
												<CommittedProject />
											</div>
										</div>
									</div>
								</div>
							</div>
						</div>
					</div>
				</div>
			</div>,
		);
		act(() => collector.enable());
		try {
			const result = collector.capture("project-1");
			expect(result.geometry.omittedCount).toBe(0);
			expect(Reflect.get(result.geometry, "ancestorTruncatedCount")).toBeGreaterThan(0);
			expect(result.geometry.nodes.every((node) => node.ancestors.length <= 8)).toBe(true);
		} finally {
			act(() => collector.disable());
		}
	});
	it("marks route/query changes partial and keeps the first cache evidence", () => {
		const queryClient = new QueryClient();
		queryClient.setQueryData(["workspaces"], [{ id: "project-1", sessions: [{ id: "worker-1", kind: "worker" }] }]);
		let revision = 1;
		const collector = createAppDiagnosticsCollector({
			queryClient,
			readRoute: () => ({ routeId: "board", projectId: "project-1", sessionId: "", hostId: "", source: "local", revision }),
		});
		render(<CommittedProject />);
		act(() => collector.enable());
		vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementationOnce(() => {
			revision++;
			queryClient.setQueryData(["workspaces"], [{ id: "project-1", sessions: [{ id: "worker-2", kind: "worker" }] }]);
			return new DOMRect();
		});
		try {
			const result = collector.capture("project-1");
			expect(result.consistency).toBe("changed");
			expect(result.route.revision).toBe(1);
			expect(result.cache.queries[0].workspaces[0].sessions[0].id).toBe("worker-1");
		} finally {
			act(() => collector.disable());
		}
	});
	it("uses exact cache keys, keeps source identities and marks omitted session ids", () => {
		const { queryClient, collector } = setup();
		queryClient.setQueryData(["workspaces", "canary-secret"], [{ token: "canary-secret" }]);
		queryClient.setQueryData(["remote-workspaces", "host-1"], [{ id: "canary-secret", path: "/canary-secret" }]);
		queryClient.setQueryData(
			["workspaces"],
			[
				{ id: "project-1", sessions: Array.from({ length: 201 }, (_, i) => ({ id: `worker-${i}`, kind: "worker" })) },
				{ id: "project-1", hostId: "host-1", sessions: [] },
				{ id: "other-project", sessions: [{ id: "other-worker" }] },
			],
		);
		collector.enable();
		try {
			const result = collector.capture("project-1");
			const rows = result.cache.queries.find((q) => q.kind === "workspaces")?.workspaces;
			expect(rows?.map((row) => [row.projectId, row.hostId, row.source])).toContainEqual(["project-1", "host-1", "remote"]);
			expect(rows?.[0].sessions).toHaveLength(200);
			expect(result.cache.omittedCount).toBeGreaterThan(0);
			expect(rows?.find((row) => row.projectId === "other-project")?.scope).toBe("counts");
			expect(JSON.stringify(result)).not.toContain("canary-secret");
		} finally {
			collector.disable();
		}
	});
});
