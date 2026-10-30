import { render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { SessionAccountControl } from "./SessionAccountControl";
import type { WorkspaceSession } from "../types/workspace";
const binding = vi.hoisted(() => ({
	accountId: "bound-a",
	provider: "codex",
	source: "explicit",
	label: "bound-a@example.test",
	available: true,
}));
vi.mock("../hooks/useAgentAccounts", () => ({
	agentAccountProviderFor: () => "codex",
	useSessionAgentAccounts: () => ({
		data: { bindings: { codex: binding } },
		isError: false,
		isLoading: false,
	}),
	useAgentAccountActions: () => ({ refreshSession: vi.fn() }),
}));
vi.mock("../hooks/useSessionInterfaceTransition", () => ({
	interfaceTransitionIsActive: () => false,
	interfaceTransitionIsAccountMove: () => false,
	useSessionInterfaceTransitionStatus: () => ({}),
}));
vi.mock("./SessionAccountSummary", () => ({ SessionAccountSummary: () => null }));
const session: WorkspaceSession = {
	id: "fake-session",
	workspaceId: "fake-project",
	workspaceName: "Fake",
	title: "Fake",
	provider: "codex",
	kind: "worker",
	status: "working",
	mode: "tui",
	prs: [],
	createdAt: "2026-10-30T00:00:00Z",
	updatedAt: "2026-10-30T00:00:00Z",
};
it("shows the durable binding label and availability without reading a default account", () => {
	const view = render(<SessionAccountControl session={session} />);
	expect(screen.getByRole("button", { name: "Account: bound-a@example.test" })).toBeVisible();
	binding.available = false;
	view.rerender(<SessionAccountControl session={session} />);
	expect(screen.getByText("bound-a@example.test")).toHaveClass("text-warning");
});
