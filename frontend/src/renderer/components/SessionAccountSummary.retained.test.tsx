import { render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import type { WorkspaceSession } from "../types/workspace";
import { SessionAccountSummary } from "./SessionAccountSummary";

vi.mock("../hooks/useAgentAccounts", async (importOriginal) => ({
	...(await importOriginal<typeof import("../hooks/useAgentAccounts")>()),
	agentAccountProviderFor: () => "claude",
	useAgentAccounts: () => ({ data: [] }),
	useSessionAgentAccounts: () => ({ data: {} }),
	useAgentAccountActions: () => ({ refreshSession: vi.fn(), switchSession: vi.fn() }),
	useAgentAccountLabel: () => vi.fn(),
	agentAccountName: () => "unused",
}));
vi.mock("../hooks/useSessionInterfaceTransition", async (importOriginal) => ({
	...(await importOriginal<typeof import("../hooks/useSessionInterfaceTransition")>()),
	useSessionInterfaceTransitionStatus: () => ({
		transition: {
			id: "redacted-move",
			sessionId: "session-1",
			sourceMode: "tui",
			targetMode: "tui",
			policy: "drain",
			historyPolicy: "strict",
			phase: "target_starting",
			accountMove: true,
			createdAt: "2026-10-01T00:00:00Z",
			updatedAt: "2026-10-01T00:00:01Z",
		},
	}),
}));

it("shows a generic account move status without any account identity", () => {
	const session: WorkspaceSession = {
		id: "session-1",
		workspaceId: "project-1",
		workspaceName: "Project",
		title: "Session",
		provider: "claude-code",
		kind: "worker",
		status: "exited",
		mode: "tui",
		prs: [],
		createdAt: "2026-10-01T00:00:00Z",
		updatedAt: "2026-10-01T00:00:01Z",
	};
	render(<SessionAccountSummary session={session} />);
	expect(screen.getByRole("status")).toHaveTextContent("Switching account");
});

it("keeps an ended session's account summary readable without offering a move", () => {
	const session: WorkspaceSession = {
		id: "session-ended",
		workspaceId: "project-1",
		workspaceName: "Project",
		title: "Session",
		provider: "claude-code",
		kind: "worker",
		status: "terminated",
		isTerminated: true,
		mode: "tui",
		prs: [],
		createdAt: "2026-10-01T00:00:00Z",
		updatedAt: "2026-10-01T00:00:01Z",
	};
	render(<SessionAccountSummary session={session} />);
	expect(screen.getByTestId("session-account-summary")).toBeVisible();
	expect(screen.getByRole("combobox", { name: "Account" })).toBeDisabled();
});
