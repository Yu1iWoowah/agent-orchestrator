import { render, screen } from "@testing-library/react";
import { useEffect } from "react";
import { describe, expect, it, vi } from "vitest";
import type { ProjectOrchestratorAction } from "../hooks/useProjectOrchestratorAction";
import { ProjectBoardActions } from "./ProjectBoardActions";
import { TooltipProvider } from "./ui/tooltip";

const attentionMounts: string[] = [];
vi.mock("./ProjectAttention", () => ({
	ProjectAttention: ({ projectId }: { projectId: string }) => {
		useEffect(() => {
			attentionMounts.push(projectId);
		}, []);
		return <button type="button" aria-label="Open project attention" />;
	},
}));

const actions: ProjectOrchestratorAction = {
	projectId: undefined,
	hostId: undefined,
	orchestrator: undefined,
	isSpawning: false,
	isProjectRestarting: false,
	isProvisioning: false,
	spawnError: "",
	canCreateAsTui: false,
	openNewTask: vi.fn(),
	openOrchestrator: vi.fn(),
};

describe("ProjectBoardActions", () => {
	it("keeps New task the one filled header action on a cloud board", () => {
		render(
			<TooltipProvider>
				<ProjectBoardActions actions={actions} cloud placement="header" />
			</TooltipProvider>,
		);
		expect(screen.getByRole("button", { name: "New task" })).toHaveClass("button-raised", "text-primary-foreground");
		expect(screen.getByRole("button", { name: /orchestrator/i })).toHaveClass("before:bg-interactive-hover");
		expect(screen.getByRole("button", { name: /orchestrator/i })).not.toHaveClass("button-raised");
		// Orchestrator leads, matching the session top bar.
		const buttons = screen.getAllByRole("button");
		expect(buttons.indexOf(screen.getByRole("button", { name: /orchestrator/i }))).toBeLessThan(buttons.indexOf(screen.getByRole("button", { name: "New task" })));
	});

	it("keeps New task the one filled header action on an empty local board", () => {
		render(
			<TooltipProvider>
				<ProjectBoardActions actions={actions} placement="header" quiet />
			</TooltipProvider>,
		);
		expect(screen.getByRole("button", { name: "New task" })).toHaveClass("button-raised", "text-primary-foreground");
		expect(screen.getByRole("button", { name: /orchestrator/i })).toHaveClass("before:bg-interactive-hover");
		expect(screen.getByRole("button", { name: /orchestrator/i })).not.toHaveClass("button-raised");
		// Orchestrator leads, matching the session top bar.
		const buttons = screen.getAllByRole("button");
		expect(buttons.indexOf(screen.getByRole("button", { name: /orchestrator/i }))).toBeLessThan(buttons.indexOf(screen.getByRole("button", { name: "New task" })));
	});

	it("remounts the attention entry for each project so nothing carries over", () => {
		attentionMounts.length = 0;
		const view = render(
			<TooltipProvider>
				<ProjectBoardActions actions={{ ...actions, projectId: "p1" }} placement="header" />
			</TooltipProvider>,
		);
		view.rerender(
			<TooltipProvider>
				<ProjectBoardActions actions={{ ...actions, projectId: "p2" }} placement="header" />
			</TooltipProvider>,
		);
		expect(attentionMounts).toEqual(["p1", "p2"]);
	});
});
