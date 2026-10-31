import { renderHook } from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import type { WorkspaceSession, WorkspaceSummary } from "../types/workspace";
import { useFollowProjectOrchestrator } from "./useFollowProjectOrchestrator";

const route = vi.hoisted(() => ({
	params: { projectId: "project", sessionId: "old", hostId: undefined as string | undefined },
	search: { followOrchestrator: true as boolean | undefined },
	navigate: vi.fn(),
}));
vi.mock("@tanstack/react-router", () => ({
	useParams: () => route.params,
	useSearch: () => route.search,
	useNavigate: () => route.navigate,
}));
function session(id: string, isTerminated = false): WorkspaceSession {
	return { id, workspaceId: "project", workspaceName: "Project", updatedAt: "2026-10-31", prs: [], kind: "orchestrator", provider: "claude-code", status: isTerminated ? "terminated" : "idle", isTerminated, title: id, createdAt: id === "old" ? "2026-10-01" : "2026-10-31" };
}
function workspace(sessions: WorkspaceSession[], hostId?: string): WorkspaceSummary {
	return { id: "project", name: "Project", path: "/project", sessions, hostId };
}
beforeEach(() => {
	route.params = { projectId: "project", sessionId: "old", hostId: undefined };
	route.search = { followOrchestrator: true };
	route.navigate.mockClear();
});
it("keeps the old transcript during kill/spawn and follows the new orchestrator without a click", () => {
	const { rerender } = renderHook(({ sessions }) => useFollowProjectOrchestrator([workspace(sessions)], []), { initialProps: { sessions: [session("old")] } });
	expect(route.navigate).not.toHaveBeenCalled();
	rerender({ sessions: [session("old", true)] });
	expect(route.navigate).not.toHaveBeenCalled();
	rerender({ sessions: [session("old", true), session("new")] });
	expect(route.navigate).toHaveBeenCalledWith({ to: "/projects/$projectId/sessions/$sessionId", params: { projectId: "project", sessionId: "new" }, search: { followOrchestrator: true }, replace: true });
});
it("follows a replacement successor and keeps following subsequent handovers", () => {
	const { rerender } = renderHook(({ sessions }) => useFollowProjectOrchestrator([workspace(sessions)], []), { initialProps: { sessions: [session("old")] } });
	rerender({ sessions: [session("old", true), session("new")] });
	expect(route.navigate).toHaveBeenCalledTimes(1);
	route.params.sessionId = "new";
	rerender({ sessions: [session("new", true), session("next")] });
	expect(route.navigate).toHaveBeenLastCalledWith(expect.objectContaining({ params: { projectId: "project", sessionId: "next" } }));
});
it("leaves an explicitly opened historical orchestrator and its transcript in place", () => {
	route.search.followOrchestrator = undefined;
	const { rerender } = renderHook(({ sessions }) => useFollowProjectOrchestrator([workspace(sessions)], []), { initialProps: { sessions: [session("old", true), session("new")] } });
	rerender({ sessions: [session("old", true), session("new", true), session("next")] });
	expect(route.navigate).not.toHaveBeenCalled();
});
it("does not follow updates after leaving for another project or its board", () => {
	const { rerender } = renderHook(() => useFollowProjectOrchestrator([workspace([session("old", true), session("new")])], []));
	route.navigate.mockClear();
	route.params.projectId = "other";
	rerender();
	expect(route.navigate).not.toHaveBeenCalled();
	route.params.projectId = "project";
	route.params.sessionId = "";
	rerender();
	expect(route.navigate).not.toHaveBeenCalled();
});
it("scopes following to the owning host when project IDs collide", () => {
	route.params.hostId = "host-b";
	renderHook(() => useFollowProjectOrchestrator([workspace([session("local")])], [workspace([session("wrong")], "host-a"), workspace([session("remote")], "host-b")]));
	expect(route.navigate).toHaveBeenCalledWith({ to: "/host/$hostId/project/$projectId/session/$sessionId", params: { hostId: "host-b", projectId: "project", sessionId: "remote" }, search: { followOrchestrator: true }, replace: true });
});
