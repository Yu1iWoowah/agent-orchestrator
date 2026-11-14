import { describe, expect, it } from "vitest";
import { projectNavigateTarget, questionNavigateTarget, sessionNavigateTarget } from "./navigate-to-session";

describe("sessionNavigateTarget", () => {
	it("keeps local URLs and identifies the remote host in a session URL", () => {
		expect(sessionNavigateTarget("project-1", "session-1")).toEqual({
			to: "/projects/$projectId/sessions/$sessionId",
			params: { projectId: "project-1", sessionId: "session-1" },
		});
		expect(sessionNavigateTarget("project-1", "session-1", "machine-a")).toEqual({
			to: "/host/$hostId/project/$projectId/session/$sessionId",
			params: { hostId: "machine-a", projectId: "project-1", sessionId: "session-1" },
		});
		expect(sessionNavigateTarget("__standalone__", "standalone-1")).toEqual({
			to: "/sessions/$sessionId",
			params: { sessionId: "standalone-1" },
		});
	});
});

it("routes a project to its owning host without changing the local URL", () => {
	expect(projectNavigateTarget("project-1")).toEqual({ to: "/projects/$projectId", params: { projectId: "project-1" } });
	expect(projectNavigateTarget("project-1", "machine-a")).toEqual({
		to: "/host/$hostId/project/$projectId", params: { hostId: "machine-a", projectId: "project-1" },
	});
});

it.each(["local", "machine-a"])("keeps the original question scope when its presentation moves on %s", (hostId) => {
	const ref = { hostId, projectId: "project-1", originSessionId: "worker", questionId: "ask-worker" };
	expect(questionNavigateTarget(ref)).toEqual({ ...projectNavigateTarget("project-1", hostId), search: { question: ref } });
	expect(questionNavigateTarget(ref, "main")).toEqual({ ...sessionNavigateTarget("project-1", "main", hostId), search: { followOrchestrator: true, question: ref } });
});
