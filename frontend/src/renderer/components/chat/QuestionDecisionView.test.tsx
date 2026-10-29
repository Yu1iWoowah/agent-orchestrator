import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { sessionQuestionFixture } from "../../test/fixtures/session-question";
import { questionSourceQueryOptions } from "../../hooks/useSessionQuestions";
import { questionDraftStorageKey, useQuestionDraftStore } from "../../stores/question-draft-store";
import { QuestionDecisionView } from "./QuestionDecisionView";
import type { SessionQuestion } from "../../lib/session-questions";

const { post } = vi.hoisted(() => ({ post: vi.fn() }));
vi.mock("../../lib/host-clients", () => ({ clientForSessionHost: () => ({ POST: (path: string, request: unknown) => post(path, request) }), labelForHost: () => "Remote host" }));
let client: QueryClient;
beforeEach(() => {
	localStorage.clear(); useQuestionDraftStore.setState({ entries: {}, storageAvailable: true });
	post.mockReset(); client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	post.mockImplementation((_path, request) => Promise.resolve({ data: { clientAnswerId: request.body.clientAnswerId, questionVersion: request.body.expectedVersion, answers: request.body.answers, savedAt: "2026-10-29T01:00:00Z", deliveryState: "queued" }, response: { status: 200 } }));
});
afterEach(() => { cleanup(); client.clear(); });

function Subject({ question = sessionQuestionFixture(), online = true, onClose = () => {} }: { question?: SessionQuestion; online?: boolean; onClose?: () => void }) {
	const query = useQuery({ ...questionSourceQueryOptions({ sessionId: question.originSessionId, questionId: question.id }), enabled: false, initialData: question });
	return <QuestionDecisionView question={query.data} submitAllowed={online} onClose={onClose} />;
}
function show(props: Parameters<typeof Subject>[0] = {}) {
	return render(<QueryClientProvider client={client}><Subject {...props} /></QueryClientProvider>);
}

it("shows descriptions and one recommendation with no preselected choice", async () => {
	show();
	const stage = await screen.findByRole("radio", { name: /Staging/ });
	expect(stage).not.toBeChecked();
	for (const control of [...screen.getAllByRole("radio"), ...screen.getAllByRole("checkbox")]) expect(control).not.toBeChecked();
	expect(screen.getAllByText("Recommended")).toHaveLength(1);
	expect(screen.getByText("Validate with internal users first.")).toBeVisible();
	expect(screen.getByText("Run automated regression checks.")).toBeVisible();
});

it("preserves choices and warns if another window removes an open editing draft", async () => {
	show();
	await userEvent.click(await screen.findByRole("radio", { name: /Staging/ }));
	const key = Object.keys(useQuestionDraftStore.getState().entries)[0];
	localStorage.removeItem(`${questionDraftStorageKey}:${key}`);
	act(() => useQuestionDraftStore.getState().syncStorage(`${questionDraftStorageKey}:${key}`, null));
	await waitFor(() => expect(screen.getByRole("radio", { name: /Staging/ })).toBeEnabled());
	expect(screen.getByRole("radio", { name: /Staging/ })).toBeChecked();
	expect(screen.getByText("Your draft is kept in this window, but could not be saved for a restart.")).toBeVisible();
});

it("does not recreate a removed draft from stale offline source data", async () => {
	show({ online: false });
	await screen.findByRole("radio", { name: /Staging/ });
	const key = Object.keys(useQuestionDraftStore.getState().entries)[0];
	act(() => useQuestionDraftStore.getState().syncStorage(`${questionDraftStorageKey}:${key}`, null));
	expect(useQuestionDraftStore.getState().entries[key]).toBeUndefined();
	expect(screen.getByRole("button", { name: "Submit all answers" })).toBeDisabled();
});

it("opens preview separately from selecting an option and submits the complete group once", async () => {
	show(); const user = userEvent.setup();
	const stage = await screen.findByRole("radio", { name: /Staging/ });
	await user.click(screen.getByText("Preview: Staging")); expect(stage).not.toBeChecked();
	await user.click(stage);
	const checks = screen.getByRole("group", { name: /Which checks/ });
	await user.click(within(checks).getByRole("checkbox", { name: "Review" }));
	await user.click(within(checks).getByRole("checkbox", { name: "Other" }));
	await user.type(screen.getByRole("textbox", { name: "Other answer: Checks" }), "Check rollback too");
	await user.click(screen.getByRole("button", { name: "Submit all answers" }));
	await waitFor(() => expect(post).toHaveBeenCalledTimes(1));
	expect(post.mock.calls[0][1].body).toMatchObject({ questionId: "ask-1", expectedVersion: 1, answers: [
		{ questionKey: "target", selectedOptionIds: ["stage"] },
		{ questionKey: "checks", selectedOptionIds: ["review"], otherText: "Check rollback too" },
	] });
	await screen.findByText("Saved · Pending delivery");
	expect(screen.queryByRole("button", { name: "Submit all answers" })).not.toBeInTheDocument();
});

it("lists errors and focuses the first invalid question without posting a partial answer", async () => {
	show(); const user = userEvent.setup();
	await user.click(await screen.findByRole("button", { name: "Submit all answers" }));
	expect(post).not.toHaveBeenCalled();
	expect(screen.getAllByText("Choose an option or enter another answer.")).toHaveLength(2);
	expect(screen.getByRole("group", { name: /Where should/ })).toHaveFocus();
});

it("keeps textarea Enter as a newline and retains unselected Other text", async () => {
	show(); const user = userEvent.setup();
	const target = await screen.findByRole("group", { name: /Where should/ });
	await user.click(within(target).getByRole("radio", { name: "Other" }));
	await user.click(within(screen.getByRole("group", { name: /Which checks/ })).getByRole("checkbox", { name: "Tests" }));
	const other = screen.getByRole("textbox", { name: "Other answer: Target" });
	await user.type(other, "First line{Enter}Second line");
	expect(other).toHaveValue("First line\nSecond line"); expect(post).not.toHaveBeenCalled();
	await user.click(within(target).getByRole("radio", { name: /Staging/ }));
	expect(screen.queryByRole("textbox", { name: "Other answer: Target" })).not.toBeInTheDocument();
	await user.click(within(target).getByRole("radio", { name: "Other" }));
	expect(screen.getByRole("textbox", { name: "Other answer: Target" })).toHaveValue("First line\nSecond line");
});

it("shows Unicode Other limits as a field error", async () => {
	show(); const user = userEvent.setup();
	const target = await screen.findByRole("group", { name: /Where should/ });
	await user.click(within(target).getByRole("radio", { name: "Other" }));
	fireEvent.change(screen.getByRole("textbox", { name: "Other answer: Target" }), { target: { value: "🚀".repeat(4001) } });
	await user.click(screen.getByRole("button", { name: "Submit all answers" }));
	expect(screen.getByText("Use at most 4000 characters for this answer.")).toBeVisible(); expect(post).not.toHaveBeenCalled();
});

it("retains the draft across close and remount, but starts v2 without copied consent", async () => {
	const first = show(); const user = userEvent.setup();
	await user.click(await screen.findByRole("radio", { name: /Staging/ }));
	first.unmount(); show();
	expect(await screen.findByRole("radio", { name: /Staging/ })).toBeChecked();
	act(() => client.setQueryData(questionSourceQueryOptions({ sessionId: "session", questionId: "ask-1" }).queryKey, sessionQuestionFixture({ version: 2 })));
	await waitFor(() => expect(screen.getByRole("radio", { name: /Staging/ })).not.toBeChecked());
	expect(screen.getByText("Previous draft · version 1 · reference only")).toBeVisible();
});

it("allows offline drafting but disables submission of a stale source", async () => {
	show({ online: false }); const user = userEvent.setup();
	const target = await screen.findByRole("group", { name: /Where should/ });
	await user.click(within(target).getByRole("radio", { name: "Other" }));
	await user.type(screen.getByRole("textbox", { name: "Other answer: Target" }), "Offline draft");
	expect(screen.getByRole("button", { name: "Submit all answers" })).toBeDisabled();
	expect(screen.getByText("The source is offline. Your draft is kept; reload before submitting.")).toBeVisible();
});

it.each([1, 4])("saves all %i related prompts together without implicit choices", async (count) => {
 const fixture = sessionQuestionFixture();
 fixture.payload = { ...fixture.payload, questions: Array.from({length: count}, (_, index) => ({ ...fixture.payload.questions[0], key: `target${index}`, header: `Target ${index}`, question: `Where should target ${index} go?` })) };
 show({ question: fixture });
 const user = userEvent.setup();
 const groups = await screen.findAllByRole("group", { name: /Where should target/ });
 expect(groups).toHaveLength(count);
 expect(screen.queryAllByRole("radio", { checked: true })).toHaveLength(0);
 for (const group of groups) await user.click(within(group).getByRole("radio", { name: /Staging/ }));
 await user.click(screen.getByRole("button", { name: "Submit all answers" }));
 await waitFor(() => expect(post).toHaveBeenCalledTimes(1));
 expect(post.mock.calls[0][1].body.answers).toEqual(Array.from({length: count},(_,index)=>({questionKey:`target${index}`,selectedOptionIds:["stage"]})));
});
