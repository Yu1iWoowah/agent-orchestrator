import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useRef, useState } from "react";
import { I18nextProvider } from "react-i18next";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { DecisionDialog } from "./DecisionDialog";
import { sessionQuestionFixture } from "../../test/fixtures/session-question";
import { useQuestionDraftStore } from "../../stores/question-draft-store";
import { questionSourceQueryOptions } from "../../hooks/useSessionQuestions";
import { createFullAppI18n } from "../../i18n/all-messages";
import type { DecisionTarget } from "../../lib/decision-target";
import { useUiStore } from "../../stores/ui-store";

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));
vi.mock("../../lib/host-clients", () => ({ clientForSessionHost: (host?: string) => ({ GET: (path: string, request: unknown) => get(host, path, request), POST: (path: string, request: unknown) => post(host, path, request) }), labelForHost: () => "Remote computer" }));
const questionTarget: DecisionTarget = { kind: "agent_question", questionId: "ask-1", title: "Deployment" };
const savedReceipt = { clientAnswerId: "external-answer", questionVersion: 1,
	answers: [{ questionKey: "target", selectedOptionIds: ["stage"] }, { questionKey: "checks", selectedOptionIds: ["test"] }],
	savedAt: "2026-10-29T01:00:00Z", deliveryState: "queued" as const };
const providerTarget: DecisionTarget = { kind: "provider_approval", activityId: "native", requestId: "rpc-native", conversationId: "conv", branchId: "root", sourceBranchId: "root", title: "Deploy command" };
const providerPage = { conversationId: "conv", activeBranchId: "root", counts: { open: 1, unread: 1, history: 0 }, hasMoreBefore: false,
	items: [{ branchId: "root", actionable: true, read: false, activity: { kind: "activity", id: "native", requestId: "rpc-native", activityKind: "approval", status: "pending", sequence: 1, revision: 0, summary: "Deploy command", detail: { message: "Deploying changes the public service.", command: "deploy production" }, createdAt: "2026-10-29T00:00:00Z" } }] };
let client: QueryClient; let surface: HTMLElement;
const locate = vi.fn(); const outsideEscape = vi.fn();
beforeEach(() => {
	localStorage.clear(); useQuestionDraftStore.setState({ entries: {}, storageAvailable: true });
	get.mockReset(); post.mockReset(); locate.mockReset(); outsideEscape.mockReset(); useUiStore.getState().clearGlobalToast();
	client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	surface = document.createElement("section"); document.body.append(surface);
	vi.spyOn(surface, "getBoundingClientRect").mockReturnValue(new DOMRect(200, 100, 1000, 800));
	get.mockImplementation((_host, path) => Promise.resolve({ data: path.includes("/questions/") ? sessionQuestionFixture() : providerPage, response: { status: 200 } }));
	post.mockResolvedValue({ response: { status: 204 } });
});
afterEach(() => { cleanup(); client.clear(); surface.remove(); });

function Harness({ target = questionTarget, hostId, rect = { left: 16, top: 48, width: 700, height: 600 } }: { target?: DecisionTarget; hostId?: string; rect?: { left: number; top: number; width: number; height: number } }) {
	const [open, setOpen] = useState(false); const trigger = useRef<HTMLButtonElement>(null);
	return <div onKeyDown={(event) => { if (event.key === "Escape") outsideEscape(); }}>
		<button ref={trigger} onClick={() => setOpen(true)}>Open decision</button>
		<DecisionDialog target={open ? target : undefined} sessionId="session" hostId={hostId} sourceAgent="claude-code" bounds={{ surface, rect }} onClose={() => setOpen(false)} onLocate={locate} onRestoreFocus={() => trigger.current?.focus()} />
	</div>;
}
function show(props: Parameters<typeof Harness>[0] = {}, locale: "en" | "zh-TW" = "en") {
	return render(<I18nextProvider i18n={createFullAppI18n(locale)}><QueryClientProvider client={client}><Harness {...props} /></QueryClientProvider></I18nextProvider>);
}

it("does not show a cached form until the latest revision finishes loading", async () => {
	client.setQueryData(questionSourceQueryOptions({ sessionId: "session", questionId: "ask-1" }).queryKey, sessionQuestionFixture());
	let finish: (value: unknown) => void = () => {};
	get.mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
	show(); await userEvent.click(screen.getByRole("button", { name: "Open decision" }));
	expect(screen.getByRole("dialog")).toBeVisible(); expect(screen.queryByRole("radio")).not.toBeInTheDocument();
	expect(screen.getByText("Loading the latest request…")).toBeVisible();
	await act(async () => finish({ data: sessionQuestionFixture({ version: 2 }), response: { status: 200 } }));
	await screen.findByRole("radio", { name: /Staging/ }); expect(screen.getByText(/Version 2/)).toBeVisible();
});

it("focuses the title, traps Tab, and closes only this dialog with Escape while keeping the draft", async () => {
	show(); const user = userEvent.setup(); await user.click(screen.getByRole("button", { name: "Open decision" }));
	const dialog = await screen.findByRole("dialog");
	await waitFor(() => expect(within(dialog).getByRole("heading", { name: "Decision" })).toHaveFocus());
	await screen.findByRole("radio", { name: /Staging/ });
	for (let index = 0; index < 12; index++) { await user.tab(); expect(dialog.contains(document.activeElement)).toBe(true); }
	for (let index = 0; index < 5; index++) { await user.tab({ shift: true }); expect(dialog.contains(document.activeElement)).toBe(true); }
	await user.click(screen.getByRole("radio", { name: /Staging/ }));
	await user.keyboard("{Escape}");
	await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
	await waitFor(() => expect(screen.getByRole("button", { name: "Open decision" })).toHaveFocus());
	expect(outsideEscape).not.toHaveBeenCalled();
	await user.click(screen.getByRole("button", { name: "Open decision" }));
	expect(await screen.findByRole("radio", { name: /Staging/ })).toBeChecked();
});

it("uses radio arrows and checkbox Space without a global Enter shortcut", async () => {
	show(); const user = userEvent.setup(); await user.click(screen.getByRole("button", { name: "Open decision" }));
	const stage = await screen.findByRole("radio", { name: /Staging/ }); stage.focus();
	await user.keyboard("{ArrowDown}"); expect(screen.getByRole("radio", { name: "Production" })).toBeChecked();
	const test = screen.getByRole("checkbox", { name: "Tests" }); test.focus(); await user.keyboard(" "); expect(test).toBeChecked();
	stage.focus(); await user.keyboard("{Enter}");
	expect(post.mock.calls.every(([, path]) => path.endsWith("/read"))).toBe(true);
});

it("keeps provider approval as a summary and navigates to the one original form", async () => {
	show({ target: providerTarget, hostId: "remote" }); const user = userEvent.setup(); await user.click(screen.getByRole("button", { name: "Open decision" }));
	await screen.findByText("Deploying changes the public service.");
	expect(screen.queryByRole("radio")).not.toBeInTheDocument(); expect(screen.queryByRole("checkbox")).not.toBeInTheDocument(); expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
	expect(screen.queryByText("Other")).not.toBeInTheDocument();
	await user.click(screen.getByRole("button", { name: "Go to original request" }));
	await waitFor(() => expect(locate).toHaveBeenCalledWith("native"));
	expect(get).toHaveBeenCalledWith("remote", "/api/v1/sessions/{sessionId}/attention/{activityId}", expect.objectContaining({ params: { path: { sessionId: "session", activityId: "native" } } }));
	expect(post.mock.calls.every(([, path]) => path.endsWith("/read"))).toBe(true);
});

it("does not navigate a provider request after its branch or request identity changes", async () => {
	get.mockResolvedValue({ data: { ...providerPage, activeBranchId: "different" }, response: { status: 200 } });
	show({ target: providerTarget }); await userEvent.click(screen.getByRole("button", { name: "Open decision" }));
	await screen.findByText("This request is no longer available.");
	expect(screen.queryByRole("button", { name: "Go to original request" })).not.toBeInTheDocument(); expect(locate).not.toHaveBeenCalled();
});

it("keeps the content rectangle in R and preserves composer space", async () => {
	show(); await userEvent.click(screen.getByRole("button", { name: "Open decision" }));
	const dialog = screen.getByRole("dialog");
	expect(dialog).toHaveStyle({ left: "566px", top: "448px", width: "560px", maxHeight: "600px" });
});

it("keeps an entry and asks for space rather than mounting an overflowing form", async () => {
	show({ rect: { left: 16, top: 48, width: 250, height: 150 } }); await userEvent.click(screen.getByRole("button", { name: "Open decision" }));
	await waitFor(() => expect(useUiStore.getState().globalToast?.title).toBe("Enlarge the Chat area to open the decision dialog. Your draft is kept."));
	expect(screen.queryByRole("dialog")).not.toBeInTheDocument(); expect(screen.getByRole("button", { name: "Open decision" })).toBeVisible();
});

it("renders worker reports as read-only fixtures without fetching or inventing choices", async () => {
	show({ target: { kind: "worker_report", reportId: "report-1", originSessionId: "worker-1", originHarness: "codex", title: "Audit summary", explanation: "The worker is waiting for the orchestrator." } });
	await userEvent.click(screen.getByRole("button", { name: "Open decision" }));
	await screen.findByText("This report is read-only. It is not an answerable question.");
	expect(screen.queryByRole("radio")).not.toBeInTheDocument(); expect(screen.queryByRole("button", { name: "Submit all answers" })).not.toBeInTheDocument(); expect(get).not.toHaveBeenCalled();
});

it("uses Traditional Chinese UI words while retaining agent and session identifiers", async () => {
	show({}, "zh-TW"); await userEvent.click(screen.getByRole("button", { name: "Open decision" }));
	await screen.findByRole("radio", { name: /Staging/ });
	expect(screen.getByRole("heading", { name: "決策對話方塊" })).toBeVisible(); expect(screen.getByText("推薦")).toBeVisible();
	expect(screen.getByRole("button", { name: "一起送出答案" })).toBeVisible(); expect(screen.getByText("來源：session · claude-code")).toBeVisible();
});

it.each([undefined, "remote"])("closes after saving an answer on host %s and restores focus", async (hostId) => {
	let question = sessionQuestionFixture();
	get.mockImplementation(() => Promise.resolve({ data: question, response: { status: 200 } }));
	post.mockImplementation((_host, path, request) => {
		if (!path.endsWith("/answer")) return Promise.resolve({ response: { status: 204 } });
		const receipt = { clientAnswerId: request.body.clientAnswerId, questionVersion: request.body.expectedVersion,
			answers: request.body.answers, savedAt: "2026-10-29T01:00:00Z", deliveryState: "queued" as const };
		question = { ...question, state: "answered", answer: receipt };
		return Promise.resolve({ data: receipt, response: { status: 200 } });
	});
	show({ hostId }); const user = userEvent.setup();
	await user.click(screen.getByRole("button", { name: "Open decision" }));
	await user.click(await screen.findByRole("radio", { name: /Staging/ }));
	await user.click(screen.getByRole("checkbox", { name: "Tests" }));
	await user.click(screen.getByRole("button", { name: "Submit all answers" }));
	await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
	await waitFor(() => expect(screen.getByRole("button", { name: "Open decision" })).toHaveFocus());
	expect(question.state).toBe("answered");
});

it("keeps a rejected answer open with the selected options and Other text", async () => {
	post.mockImplementation((_host, path) => Promise.resolve(path.endsWith("/answer")
		? { error: { code: "QUESTION_INVALID", message: "Please review your answer." }, response: { status: 400 } }
		: { response: { status: 204 } }));
	show(); const user = userEvent.setup();
	await user.click(screen.getByRole("button", { name: "Open decision" }));
	const group = await screen.findByRole("group", { name: /Where should/ });
	await user.click(within(group).getByRole("radio", { name: "Other" }));
	await user.type(screen.getByRole("textbox", { name: "Other answer: Target" }), "Keep this answer");
	await user.click(screen.getByRole("checkbox", { name: "Tests" }));
	await user.click(screen.getByRole("button", { name: "Submit all answers" }));
	await screen.findByRole("alert");
	expect(screen.getByRole("dialog")).toBeVisible();
	expect(screen.getByRole("textbox", { name: "Other answer: Target" })).toHaveValue("Keep this answer");
	expect(screen.getByRole("checkbox", { name: "Tests" })).toBeChecked();
	expect(screen.getByRole("button", { name: "Submit all answers" })).toBeEnabled();
});

it.each(["answered", "cancelled", "expired"] as const)("closes when an open question becomes %s through a source refresh", async (state) => {
	let question = sessionQuestionFixture({ read: true });
	get.mockImplementation(() => Promise.resolve({ data: question, response: { status: 200 } }));
	show(); await userEvent.click(screen.getByRole("button", { name: "Open decision" }));
	await screen.findByRole("radio", { name: /Staging/ });
	question = { ...question, state, answer: state === "answered" ? savedReceipt : undefined };
	await act(async () => { await client.invalidateQueries({ queryKey: questionSourceQueryOptions({ sessionId: "session", questionId: "ask-1" }).queryKey }); });
	await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
});

it.each(["answered", "cancelled", "expired"] as const)("keeps a %s question opened from history available to read", async (state) => {
	get.mockResolvedValue({ data: sessionQuestionFixture({ state, answer: state === "answered" ? savedReceipt : undefined }), response: { status: 200 } });
	show(); await userEvent.click(screen.getByRole("button", { name: "Open decision" }));
	await screen.findByRole("radio", { name: /Staging/ });
	expect(screen.getByRole("dialog")).toBeVisible();
	expect(screen.queryByRole("button", { name: "Submit all answers" })).not.toBeInTheDocument();
});
