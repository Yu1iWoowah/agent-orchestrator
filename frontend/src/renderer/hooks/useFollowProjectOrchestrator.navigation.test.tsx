import { useState } from "react";
import { act, render, screen, waitFor } from "@testing-library/react";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider, useParams } from "@tanstack/react-router";
import { expect, it } from "vitest";
import { Route as AppRoot } from "../routes/__root";
import { orchestratorNavigateTarget, sessionNavigateTarget } from "../lib/navigate-to-session";
import type { WorkspaceSummary } from "../types/workspace";
import { useFollowProjectOrchestrator } from "./useFollowProjectOrchestrator";

it("follows workspace updates in the real router, then clears intent when opening history", async () => {
	function workspace(active: string): WorkspaceSummary {
		return { id: "project", name: "Project", path: "/project", sessions: ["old", active].filter((id, index, all) => all.indexOf(id) === index).map((id) => ({
			id, workspaceId: "project", workspaceName: "Project", title: id, kind: "orchestrator", provider: "claude-code", status: "idle", isTerminated: id !== active, createdAt: id === "old" ? "2026-10-01" : "2026-10-31", updatedAt: "2026-10-31", prs: [],
		})) };
	}
	let publish = (_active: string) => {};
	const root = createRootRoute({
		validateSearch: AppRoot.options.validateSearch,
		component: function Root() {
			const [active, setActive] = useState("old");
			publish = setActive;
			useFollowProjectOrchestrator([workspace(active)], []);
			return <Outlet />;
		},
	});
	const sessionRoute = createRoute({
		getParentRoute: () => root,
		path: "/projects/$projectId/sessions/$sessionId",
		component: function Session() {
			const { sessionId } = useParams({ strict: false });
			return <p>Transcript: {sessionId}</p>;
		},
	});
	const router = createRouter({ routeTree: root.addChildren([sessionRoute]), history: createMemoryHistory({ initialEntries: ["/projects/project/sessions/old?followOrchestrator=history&project=keep"] }) });
	render(<RouterProvider router={router} />);
	await screen.findByText("Transcript: old");
	await act(async () => { publish("new"); });
	await waitFor(() => expect(screen.getByText("Transcript: old")).toBeVisible());
	await act(async () => { await router.navigate(orchestratorNavigateTarget("project", "old")); });
	await screen.findByText("Transcript: new");
	expect(router.state.location.search).toEqual({ followOrchestrator: true });
	await act(async () => { await router.navigate(sessionNavigateTarget("project", "old")); });
	await screen.findByText("Transcript: old");
	expect(router.state.location.search.followOrchestrator).toBeUndefined();
	expect(router.state.location.searchStr).toBe("");
	act(() => publish("next"));
	await waitFor(() => expect(screen.getByText("Transcript: old")).toBeVisible());
	expect(router.state.location.pathname).toBe("/projects/project/sessions/old");
});
