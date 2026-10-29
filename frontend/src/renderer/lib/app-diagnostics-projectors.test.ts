import { describe, expect, it } from "vitest";
import { componentObservation, projectWorkspaces } from "./app-diagnostics-projectors";
describe("diagnostic id projection and omission accounting", () => {
	it.each(["next.js", "socket.io"])("preserves project/session evidence for %s", (project) => {
		const worker = { id: `${project}-1`, kind: "worker" };
		const row = { id: project, sessions: [worker] };
		const projected = projectWorkspaces([row], new Set([project]));
		expect(projected[0]).toMatchObject({ projectId: project, scope: "ids", omittedCount: 0, sessions: [{ id: worker.id }] });
		const summary = componentObservation("project", row, {
			live: [worker],
			workers: [worker],
			ordered: [worker],
			listed: [worker],
			workspaces: [row],
		});
		expect(summary.projectId).toBe(project);
		for (const ids of [summary.liveIds, summary.workerIds, summary.orderedIds, summary.listedIds]) expect(ids).toEqual([worker.id]);
		expect(summary.omittedCount).toBe(0);
	});
	it("counts invalid session and workspace ids as omissions", () => {
		const worker = { id: "next.js-1", kind: "worker" };
		const invalid = { id: "/private/path" };
		const row = { id: "next.js", sessions: [worker, invalid] };
		expect(projectWorkspaces([row])[0].omittedCount).toBe(1);
		const result = componentObservation("project", row, {
			live: [worker, invalid],
			workers: [worker, invalid],
			ordered: [worker, invalid],
			listed: [worker, invalid],
			workspaces: [row, invalid],
		});
		expect(result.omittedCount).toBe(5);
		expect(componentObservation("project", invalid).omittedCount).toBe(1);
		expect(result.workspaces[0].omittedCount).toBe(1);
		expect(JSON.stringify(result)).not.toContain("/private/path");
	});
});
