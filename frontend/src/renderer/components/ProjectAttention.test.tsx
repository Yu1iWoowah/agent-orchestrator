import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
	cleanup,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ProjectAttention, ProjectAttentionCount } from "./ProjectAttention";
const { get, post, navigate } = vi.hoisted(() => ({
	get: vi.fn(),
	post: vi.fn(),
	navigate: vi.fn(),
}));
vi.mock("../lib/host-clients", () => ({
	clientForSessionHost: () => ({ GET: get, POST: post }),
}));
vi.mock("@tanstack/react-router", () => ({ useNavigate: () => navigate }));
vi.mock("./chat/DecisionDialog", () => ({
	DecisionDialog: ({
		target,
		onClose,
		onLocate,
	}: {
		target: { title: string; kind: string };
		onClose: () => void;
		onLocate: () => void;
	}) => (
		<div role="dialog" aria-label="Decision">
			<h2>{target.title}</h2>
			<span>{target.kind}</span>
			<button onClick={onClose}>Close decision</button>
			<button onClick={onLocate}>Original session</button>
		</div>
	),
}));
let client: QueryClient;
beforeEach(() => {
	client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	post.mockResolvedValue({ response: { status: 204 } });
});
afterEach(() => {
	cleanup();
	client.clear();
	vi.clearAllMocks();
});
const item = {
	id: "question:ask-1",
	kind: "agent_question",
	sessionId: "worker",
	sessionName: "Review worker",
	title: "Where should this run?",
	revision: 1,
	createdAt: "2026-10-29T00:00:00Z",
	read: false,
	questionId: "ask-1",
};
function show() {
	return render(
		<QueryClientProvider client={client}>
			<ProjectAttention projectId="project" />
		</QueryClientProvider>,
	);
}
it("separates reports from user questions and marks only an inspected report read", async () => {
	get.mockImplementation((_path, { params }) =>
		Promise.resolve({
			data:
				params.query.audience === "user"
					? { items: [item], open: 1, unread: 1 }
					: {
							items: [
								{
									...item,
									id: "report:r1",
									kind: "worker_report",
									reportId: "r1",
									questionId: undefined,
									originHarness: "codex",
									title: "Need orchestrator guidance",
									deliveryState: "acknowledged",
									answerState: "unknown",
								},
							],
							open: 1,
							unread: 1,
						},
			response: { status: 200 },
		}),
	);
	show();
	await waitFor(() =>
		expect(
			screen.getByRole("button", { name: "Open project attention" }),
		).toHaveTextContent("1"),
	);
	expect(navigate).not.toHaveBeenCalled();
	expect(post).not.toHaveBeenCalled();
	fireEvent.click(
		screen.getByRole("button", { name: "Open project attention" }),
	);
	expect(await screen.findByText("Where should this run?")).toBeVisible();
	fireEvent.mouseDown(
		screen.getByRole("tab", { name: "Orchestrator reports" }),
		{ button: 0, ctrlKey: false },
	);
	expect(
		await screen.findByText(
			"Answer state unknown · report delivery: acknowledged",
		),
	).toBeVisible();
	expect(post).not.toHaveBeenCalled();
	fireEvent.click(
		screen.getByRole("button", { name: /Need orchestrator guidance/ }),
	);
	expect(
		await screen.findByRole("dialog", { name: "Decision" }),
	).toHaveTextContent("worker_report");
	await waitFor(() => expect(post).toHaveBeenCalledTimes(1));
	expect(post.mock.calls[0][0]).toBe(
		"/api/v1/projects/{id}/attention/reports/{reportId}/read",
	);
	expect(navigate).not.toHaveBeenCalled();
});
it("navigates provider requests to their source session without resolving approval", async () => {
	get.mockResolvedValue({
		data: {
			items: [
				{
					...item,
					id: "provider:a",
					kind: "provider_approval",
					questionId: undefined,
					activityId: "a",
					requestId: "rpc",
					conversationId: "conversation",
					branchId: "main",
					sourceBranchId: "main",
				},
			],
			open: 1,
			unread: 1,
		},
		response: { status: 200 },
	});
	show();
	fireEvent.click(
		screen.getByRole("button", { name: "Open project attention" }),
	);
	fireEvent.click(
		await screen.findByRole("button", { name: /Where should this run/ }),
	);
	fireEvent.click(
		await screen.findByRole("button", { name: "Original session" }),
	);
	expect(navigate).toHaveBeenCalledWith({
		to: "/projects/$projectId/sessions/$sessionId",
		params: { projectId: "project", sessionId: "worker" },
	});
	expect(post).not.toHaveBeenCalled();
});
it("shows unknown sidebar counts when refresh fails and does not navigate", async () => {
	get.mockRejectedValue(new Error("offline"));
	render(
		<QueryClientProvider client={client}>
			<ProjectAttentionCount projectId="project" />
		</QueryClientProvider>,
	);
	expect(await screen.findByLabelText("Awaiting you: ?")).toHaveTextContent(
		"?",
	);
	expect(navigate).not.toHaveBeenCalled();
});
