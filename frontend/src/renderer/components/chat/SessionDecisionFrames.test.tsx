import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { SessionConversationFrames } from "./SessionConversationFrames";
import { TooltipProvider } from "../ui/tooltip";
import { sessionQuestionFixture } from "../../test/fixtures/session-question";
import { useConversationFrameStore } from "../../stores/conversation-frame-store";
import { useQuestionDraftStore } from "../../stores/question-draft-store";
import { QuestionAnswerRuntime } from "../../hooks/useQuestionAnswerRecovery";
import type { SessionQuestion } from "../../lib/session-questions";

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));
vi.mock("../../lib/host-clients", () => ({ clientForSessionHost: (host?: string) => ({ GET: (path: string, request: unknown) => get(host, path, request), POST: (path: string, request: unknown) => post(host, path, request) }), labelForHost: () => "Remote" }));
let surface: HTMLElement; let rect = { left: 16, top: 48, width: 900, height: 700 };
vi.mock("../../hooks/useConversationFrameSafeRect", () => ({ useConversationFrameSafeRect: () => ({ surface, rect }) }));
let client: QueryClient; let questions: Map<string, SessionQuestion>;
const locate = vi.fn();
const provider = { branchId: "root", actionable: true, read: false, activity: { kind: "activity", id: "native", requestId: "rpc-native", activityKind: "approval", status: "pending", sequence: 1, revision: 0, summary: "Deploy command", detail: { message: "Public deployment needs original provider approval." }, createdAt: "2026-10-29T00:00:00Z" } };
beforeEach(() => {
	localStorage.clear(); useConversationFrameStore.setState({ sessions: {} }); useQuestionDraftStore.setState({ entries: {}, storageAvailable: true });
	get.mockReset(); post.mockReset(); locate.mockReset(); rect = { left: 16, top: 48, width: 900, height: 700 };
	questions = new Map(); client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	surface = document.createElement("section"); document.body.append(surface);
	get.mockImplementation((_host, path, request) => {
		const session = request.params.path.sessionId;
		let question = questions.get(session);
		if (!question) { question = sessionQuestionFixture({ originSessionId: session }); questions.set(session, question); }
		const data = path.endsWith("/plan") ? { state: "absent", activeBranchId: "root" } : path.endsWith("/questions/{questionId}") ? question : path.endsWith("/questions") ? {
			items: request.params.query.state === "history" ? (question.state === "open" ? [] : [question]) : question.state === "open" ? [question] : [],
			open: question.state === "open" ? 1 : 0, unread: question.state === "open" && !question.read ? 1 : 0, history: question.state === "open" ? 0 : 1, hasMoreBefore: false, nextBeforeSequence: 0,
		} : { conversationId: `conv-${session}`, activeBranchId: "root", items: [provider], counts: { open: 1, unread: 1, history: 0 }, hasMoreBefore: false };
		return Promise.resolve({ data: structuredClone(data), response: { status: 200 } });
	});
	post.mockImplementation((_host, path, request) => {
		if (path.includes("/questions/") && path.endsWith("/read")) {
			const current = questions.get(request.params.path.sessionId);
			if (current) questions.set(request.params.path.sessionId, { ...current, read: true });
		}
		return Promise.resolve({ response: { status: 204 } });
	});
});
afterEach(() => { cleanup(); client.clear(); surface.remove(); });

function Subject({ session = "session" }: { session?: string }) {
	return <QueryClientProvider client={client}><QuestionAnswerRuntime /><TooltipProvider><SessionConversationFrames sessionId={session} activeBranchId="root" sourceAgent="claude-code" surfaceRef={{ current: surface }} composerRef={{ current: null }} onLocateAttention={locate} /></TooltipProvider></QueryClientProvider>;
}

it("counts a two-question envelope as one item and opens every row as a decision", async () => {
	render(<Subject />);
	const row = await screen.findByRole("button", { name: /Open decision: Choose a deployment/ });
	const frame = screen.getByRole("region", { name: "Awaiting you" });
	expect(within(frame).getByText("2", { exact: true })).toBeVisible(); expect(within(frame).getByText("2 questions")).toBeVisible();
	row.focus(); await userEvent.keyboard("{Enter}");
	await screen.findByRole("dialog", { name: "Decision" }); await screen.findByRole("radio", { name: /Staging/ });
	await userEvent.keyboard("{Escape}");
	await waitFor(() => expect(screen.queryByRole("dialog", { name: "Decision" })).not.toBeInTheDocument());
	expect(screen.getByRole("region", { name: "Awaiting you" })).toBeVisible();
	const restored = screen.getByRole("button", { name: /Open decision: Choose a deployment/ });
	await waitFor(() => expect(restored).toHaveFocus());
	await userEvent.keyboard(" "); await screen.findByRole("dialog", { name: "Decision" });
});

it("requires the provider summary dialog before navigating to the unique original form", async () => {
	render(<Subject />); await userEvent.click(await screen.findByRole("button", { name: /Open decision: Public deployment/ }));
	await screen.findByRole("button", { name: "Go to original request" }); expect(locate).not.toHaveBeenCalled();
	expect(screen.queryByRole("radio")).not.toBeInTheDocument(); expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
	await userEvent.click(screen.getByRole("button", { name: "Go to original request" }));
	await waitFor(() => expect(locate).toHaveBeenCalledWith("native"));
	expect(post.mock.calls.every(([, path]) => path.endsWith("/read"))).toBe(true);
});

it("suspends the compact Sheet and restores its scroll, focus and open preference", async () => {
	rect = { left: 16, top: 48, width: 300, height: 200 };
	render(<Subject />); await userEvent.click(screen.getByRole("button", { name: "Open Awaiting you" }));
	const sheet = screen.getByRole("dialog", { name: "Awaiting you" });
	const row = await within(sheet).findByRole("button", { name: /Open decision: Choose a deployment/ });
	const scroll = sheet.querySelector<HTMLElement>("[data-conversation-frame-sheet-body]"); if (!scroll) throw new Error("missing Sheet body"); scroll.scrollTop = 64;
	await userEvent.click(row);
	await screen.findByRole("dialog", { name: "Decision" });
	await waitFor(() => expect(screen.queryByRole("dialog", { name: "Awaiting you" })).not.toBeInTheDocument());
	expect(screen.getAllByRole("dialog")).toHaveLength(1);
	await screen.findByRole("radio", { name: /Staging/ }); await userEvent.keyboard("{Escape}");
	const restored = await screen.findByRole("dialog", { name: "Awaiting you" });
	await waitFor(() => expect(within(restored).getByRole("button", { name: /Open decision: Choose a deployment/ })).toHaveFocus());
	expect(restored.querySelector<HTMLElement>("[data-conversation-frame-sheet-body]")?.scrollTop).toBe(64);
	expect(useConversationFrameStore.getState().sessions[JSON.stringify(["local", "session"])]?.attention.userOpen ?? true).toBe(true);
});

it("keeps an in-flight answer scoped to the old session after closing and switching", async () => {
	const view = render(<Subject />); const user = userEvent.setup();
	await user.click(await screen.findByRole("button", { name: /Open decision: Choose a deployment/ }));
	await user.click(await screen.findByRole("radio", { name: /Staging/ })); await user.click(screen.getByRole("checkbox", { name: "Tests" }));
	let finish: () => void = () => {};
	post.mockImplementation((_host, path, request) => {
		if (!path.endsWith("/answer")) return Promise.resolve({ response: { status: 204 } });
		return new Promise((resolve) => { finish = () => {
			const answer = { clientAnswerId: request.body.clientAnswerId, questionVersion: request.body.expectedVersion, answers: request.body.answers, savedAt: "2026-10-29T01:00:00Z", deliveryState: "queued" as const };
			const current = questions.get(request.params.path.sessionId); if (!current) throw new Error("missing original question");
			questions.set(request.params.path.sessionId, { ...current, state: "answered", answer }); resolve({ data: answer, response: { status: 200 } });
		}; });
	});
	await user.click(screen.getByRole("button", { name: "Submit all answers" }));
	await user.click(screen.getByRole("button", { name: "Later" })); view.rerender(<Subject session="other-session" />);
	await user.click(await screen.findByRole("button", { name: /Open decision: Choose a deployment/ }));
	expect(await screen.findByRole("radio", { name: /Staging/ })).not.toBeChecked();
	await act(async () => finish());
	await waitFor(() => expect(Object.values(useQuestionDraftStore.getState().entries).some((entry) => entry.scope.sessionId === "session")).toBe(false));
	expect(screen.getByRole("radio", { name: /Staging/ })).not.toBeChecked();
	expect(screen.getByRole("button", { name: "Submit all answers" })).toBeEnabled();
});

it("closes to the entry instead of reopening the Sheet when the dialog no longer fits", async () => {
 rect = { left: 16, top: 48, width: 300, height: 200 };
 const view = render(<Subject />);
 await userEvent.click(screen.getByRole("button", { name: "Open Awaiting you" }));
 await userEvent.click(await screen.findByRole("button", { name: /Open decision: Choose a deployment/ }));
 await screen.findByRole("dialog", { name: "Decision" });
 rect = { left: 16, top: 48, width: 250, height: 150 };
 view.rerender(<Subject />);
 await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
 await waitFor(() => expect(screen.getByRole("button", { name: "Awaiting you 2" })).toHaveFocus());
 expect(Object.values(useQuestionDraftStore.getState().entries)).toHaveLength(1);
});
