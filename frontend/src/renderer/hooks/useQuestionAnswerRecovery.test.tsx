import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { sessionQuestionFixture } from "../test/fixtures/session-question";
import { useQuestionDraftStore } from "../stores/question-draft-store";
import { sendQuestionAnswer, useQuestionAnswerRecovery } from "./useQuestionAnswerRecovery";
import { questionSourceQueryOptions } from "./useSessionQuestions";
import type { components } from "../../api/schema";

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));
vi.mock("../lib/host-clients", () => ({ clientForSessionHost: (host?: string) => ({ GET: (path: string, request: unknown) => get(host, path, request), POST: (path: string, request: unknown) => post(host, path, request) }) }));
let client: QueryClient;
const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
beforeEach(() => {
	localStorage.clear(); useQuestionDraftStore.setState({ entries: {}, storageAvailable: true });
	get.mockReset(); post.mockReset(); client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
});
afterEach(() => { cleanup(); client.clear(); vi.useRealTimers(); });

function ready(hostId?: string) {
	const key = useQuestionDraftStore.getState().ensure(sessionQuestionFixture(), hostId);
	useQuestionDraftStore.getState().setField(key, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "" });
	useQuestionDraftStore.getState().setField(key, "checks", { selectedOptionIds: ["test"], otherSelected: false, otherText: "" });
	const answer = useQuestionDraftStore.getState().begin(key, "answer-1", Date.now());
	if (!answer) throw new Error("missing answer");
	return { key, answer };
}

function receipt(answer: components["schemas"]["AttentionAnswerSubmission"]): components["schemas"]["AttentionAnswerReceipt"] {
	return { clientAnswerId: answer.clientAnswerId, questionVersion: answer.expectedVersion, answers: answer.answers, savedAt: "2026-10-29T01:00:00Z", deliveryState: "queued" };
}

it("saves one queued receipt and never describes queued as delivered", async () => {
	const { key, answer } = ready(); post.mockResolvedValue({ data: receipt(answer), response: { status: 200 } });
	await sendQuestionAnswer(client, key);
	expect(useQuestionDraftStore.getState().entries[key]).toBeUndefined();
	const saved = client.getQueryData(questionSourceQueryOptions({ sessionId: "session", questionId: "ask-1" }).queryKey);
	expect(saved).toMatchObject({ state: "answered", answer: { clientAnswerId: "answer-1", deliveryState: "queued" } });
	expect(post).toHaveBeenCalledTimes(1);
});

it("recovers a lost response after the decision view has gone, without reposting a saved answer", async () => {
	const { key, answer } = ready(); post.mockRejectedValue(new Error("response lost"));
	await sendQuestionAnswer(client, key);
	expect(useQuestionDraftStore.getState().entries[key].phase.state).toBe("unknown");
	get.mockResolvedValue({ data: sessionQuestionFixture({ state: "answered", answer: receipt(answer) }), response: { status: 200 } });
	renderHook(() => useQuestionAnswerRecovery(), { wrapper });
	await waitFor(() => expect(useQuestionDraftStore.getState().entries[key]).toBeUndefined());
	expect(post).toHaveBeenCalledTimes(1);
});

it("retries only the frozen submission with its original ID after reading an open question", async () => {
	const { key, answer } = ready(); post.mockRejectedValueOnce(new Error("offline"));
	await sendQuestionAnswer(client, key);
	useQuestionDraftStore.getState().unknown(key, answer.clientAnswerId, "offline", Date.now() - 3000);
	get.mockResolvedValue({ data: sessionQuestionFixture(), response: { status: 200 } });
	post.mockResolvedValue({ data: receipt(answer), response: { status: 200 } });
	renderHook(() => useQuestionAnswerRecovery(), { wrapper });
	await waitFor(() => expect(useQuestionDraftStore.getState().entries[key]).toBeUndefined());
	expect(post).toHaveBeenCalledTimes(2);
	for (const [, path, request] of post.mock.calls) {
		expect(path).toBe("/api/v1/sessions/{sessionId}/questions/{questionId}/answer");
		expect(request.body).toEqual(answer);
	}
});

it("locks a 409 draft without replacing its choices", async () => {
	const { key } = ready(); post.mockResolvedValue({ error: { code: "QUESTION_CHANGED", message: "Reload question" }, response: { status: 409 } });
	await sendQuestionAnswer(client, key);
	expect(useQuestionDraftStore.getState().entries[key]).toMatchObject({ phase: { state: "conflict" }, fields: { target: { selectedOptionIds: ["stage"] } } });
});

it("keeps explicit validation failures editable with their field errors", async () => {
	const { key } = ready(); post.mockResolvedValue({ error: { code: "QUESTION_INVALID", message: "Check answer", details: { fieldErrors: { "answers.target": "Invalid target" } } }, response: { status: 400 } });
	await sendQuestionAnswer(client, key);
	expect(useQuestionDraftStore.getState().entries[key]).toMatchObject({ phase: { state: "editing", fieldErrors: { "answers.target": "Invalid target" } }, fields: { target: { selectedOptionIds: ["stage"] } } });
});

it.each(["different-client", "answer-1"])("does not mistake a competing receipt for this submission (%s)", async (id) => {
	const { key, answer } = ready(); post.mockRejectedValue(new Error("response lost"));
	await sendQuestionAnswer(client, key);
	const competing = { ...receipt(answer), clientAnswerId: id, answers: [{ questionKey: "target", selectedOptionIds: ["prod"] }, { questionKey: "checks", selectedOptionIds: ["review"] }] };
	get.mockResolvedValue({ data: sessionQuestionFixture({ state: "answered", answer: competing }), response: { status: 200 } });
	renderHook(() => useQuestionAnswerRecovery(), { wrapper });
	await waitFor(() => expect(useQuestionDraftStore.getState().entries[key].phase.state).toBe("conflict"));
	expect(useQuestionDraftStore.getState().entries[key].fields.target.selectedOptionIds).toEqual(["stage"]);
	expect(post).toHaveBeenCalledTimes(1);
});

it("leaves an unsupported host unknown rather than claiming cancellation or success", async () => {
	const { key } = ready("remote"); post.mockRejectedValue(new Error("disconnected"));
	await sendQuestionAnswer(client, key);
	get.mockResolvedValue({ error: { code: "NOT_IMPLEMENTED" }, response: { status: 501 } });
	renderHook(() => useQuestionAnswerRecovery(), { wrapper });
	await waitFor(() => expect(get).toHaveBeenCalled());
	expect(useQuestionDraftStore.getState().entries[key].phase.state).toBe("unknown");
	expect(post).toHaveBeenCalledTimes(1);
});

it("does not clear another host's draft when a late save arrives", async () => {
	const a = ready("host-a"); const b = ready("host-b");
	post.mockResolvedValue({ data: receipt(a.answer), response: { status: 200 } });
	await sendQuestionAnswer(client, a.key);
	expect(useQuestionDraftStore.getState().entries[a.key]).toBeUndefined();
	expect(useQuestionDraftStore.getState().entries[b.key].phase.state).toBe("submitting");
	expect(client.getQueryData(questionSourceQueryOptions({ sessionId: "session", questionId: "ask-1", hostId: "host-b" }).queryKey)).toBeUndefined();
});
