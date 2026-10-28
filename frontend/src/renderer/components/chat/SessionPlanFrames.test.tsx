import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionPlanFrames } from "./SessionPlanFrames";
import { TooltipProvider } from "../ui/tooltip";
import { useConversationFrameStore } from "../../stores/conversation-frame-store";

const { get } = vi.hoisted(() => ({ get: vi.fn() }));
vi.mock("../../lib/host-clients", () => ({ clientForSessionHost: () => ({ GET: get }) }));
let surface: HTMLElement;
vi.mock("../../hooks/useConversationFrameSafeRect", () => ({
	useConversationFrameSafeRect: () => ({
		surface,
		rect: { left: 16, top: 48, width: 700, height: 600 },
	}),
}));
let client: QueryClient;
const version = {
	branchId: "root",
	turnId: "",
	sourceActivityId: "source-without-turn",
	sequence: 4,
	revision: 1,
	updatedAt: "2026-10-28T12:00:00Z",
	plan: { steps: [], explanation: "Explicit clear" },
};
function show() {
	render(
		<QueryClientProvider client={client}>
			<TooltipProvider>
				<SessionPlanFrames
					sessionId="plan-states"
					surfaceRef={{ current: surface }}
					composerRef={{ current: null }}
				/>
			</TooltipProvider>
		</QueryClientProvider>,
	);
	fireEvent.click(screen.getByRole("button", { name: "Open Plan" }));
}
beforeEach(() => {
	get.mockReset();
	useConversationFrameStore.setState({ sessions: {} });
	surface = document.createElement("section");
	document.body.append(surface);
	client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
});
afterEach(() => {
	cleanup();
	client.clear();
	surface.remove();
});
describe("session Plan states", () => {
	it("keeps an open frame while the independent Plan request is loading", () => {
		get.mockImplementation(() => new Promise(() => {}));
		show();
		expect(screen.getByRole("status")).toHaveTextContent("Loading Plan");
		expect(screen.getByRole("region", { name: "Plan" })).toBeVisible();
	});
	it("distinguishes no Plan from an explicit clear with an unknown turn", async () => {
		get.mockResolvedValue({
			data: { state: "absent", activeBranchId: "root" },
			response: { status: 200 },
		});
		show();
		expect(await screen.findByText("No structured Plan yet.")).toBeVisible();
		get.mockResolvedValue({
			data: { state: "cleared", activeBranchId: "root", version },
			response: { status: 200 },
		});
		await act(async () => {
			await client.invalidateQueries({ queryKey: ["session-plan", "plan-states"] });
		});
		expect(await screen.findByText("Explicit clear")).toBeVisible();
		expect(screen.getByText("The agent cleared this Plan.")).toBeVisible();
		expect(screen.getByText(/Unknown turn/)).toBeVisible();
	});
	it("keeps offline state unknown and retries without collapsing", async () => {
		get.mockRejectedValue(new Error("offline"));
		show();
		expect(await screen.findByRole("alert")).toHaveTextContent("Could not load Plan");
		get.mockResolvedValue({
			data: { state: "absent", activeBranchId: "root" },
			response: { status: 200 },
		});
		fireEvent.click(screen.getByRole("button", { name: "Retry" }));
		expect(await screen.findByText("No structured Plan yet.")).toBeVisible();
		expect(screen.getByRole("region", { name: "Plan" })).toBeVisible();
	});
	it("shows unsupported capability separately from an empty Plan", async () => {
		get.mockResolvedValue({ error: { code: "NOT_IMPLEMENTED" }, response: { status: 501 } });
		show();
		expect(await screen.findByRole("alert")).toHaveTextContent("Plan is not supported");
		expect(screen.queryByText("No structured Plan yet.")).not.toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
	});
	it("loads saved history on demand and expands its source", async () => {
		get.mockImplementation((path: string) =>
			Promise.resolve({
				data: path.endsWith("/history")
					? { activeBranchId: "root", versions: [version], hasMoreBefore: false }
					: { state: "absent", activeBranchId: "root" },
				response: { status: 200 },
			}),
		);
		show();
		await screen.findByText("No structured Plan yet.");
		expect(get).toHaveBeenCalledTimes(1);
		await userEvent.setup().click(screen.getByRole("tab", { name: "History" }));
		await waitFor(() =>
			expect(screen.getByText(/Unknown turn/, { selector: "summary" })).toBeVisible(),
		);
		fireEvent.click(screen.getByText(/Unknown turn/, { selector: "summary" }));
		expect(screen.getByText("Explicit clear")).toBeVisible();
	});
});
