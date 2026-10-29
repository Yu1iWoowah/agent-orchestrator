import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { sessionQuestionFixture } from "../test/fixtures/session-question";
import { questionDraftStorageKey, useQuestionDraftStore } from "../stores/question-draft-store";
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

it.each(["saved", "lost"])("submits a new answer with offline-host drafts occupying all slots (%s response)", async (mode) => {
	const oldKeys: string[] = [];
	for (let i = 0; i < 32; i++) {
		const key = useQuestionDraftStore.getState().ensure(sessionQuestionFixture({ id: `offline-${i}` }), "disconnected");
		useQuestionDraftStore.getState().setField(key, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "keep this offline draft" });
		oldKeys.push(key);
	}
	const { key, answer } = ready();
	expect(useQuestionDraftStore.getState().storageAvailable).toBe(false);
	expect(localStorage.getItem(`${questionDraftStorageKey}:${key}`)).toBeNull();
	post.mockImplementation(() => mode === "lost" ? Promise.reject(new Error("response lost after save")) : Promise.resolve({ data: receipt(answer), response: { status: 200 } }));
	await sendQuestionAnswer(client, key);
	get.mockImplementation((host) => host === "disconnected" ? Promise.reject(new Error("Host disconnected is not connected"))
		: Promise.resolve({ data: sessionQuestionFixture({ state: "answered", answer: receipt(answer) }), response: { status: 200 } }));
	renderHook(() => useQuestionAnswerRecovery(), { wrapper });
	await waitFor(() => expect(get.mock.calls.filter(([host]) => host === "disconnected")).toHaveLength(32));
	await waitFor(() => expect(useQuestionDraftStore.getState().entries[key]).toBeUndefined());
	expect(post).toHaveBeenCalledTimes(1);
	expect(post.mock.calls[0][2].body).toEqual(answer);
	for (const oldKey of oldKeys) {
		expect(useQuestionDraftStore.getState().entries[oldKey].fields.target.otherText).toBe("keep this offline draft");
		expect(localStorage.getItem(`${questionDraftStorageKey}:${oldKey}`)).not.toBeNull();
	}
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

it.each(["answered", "cancelled", "expired", "deleted"] as const)("cleans an externally %s draft while keeping another pending question without opening history", async (state) => {
	const a = useQuestionDraftStore.getState().ensure(sessionQuestionFixture());
	useQuestionDraftStore.getState().setField(a, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "keep until confirmed" });
	const b = useQuestionDraftStore.getState().ensure(sessionQuestionFixture({ id: "ask-b" }));
	useQuestionDraftStore.getState().setField(b, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "" });
	get.mockImplementation((_host, _path, request) => Promise.resolve(request.params.path.questionId === "ask-b"
		? { data: sessionQuestionFixture({ id: "ask-b" }), response: { status: 200 } }
		: state === "deleted" ? { error: { code: "SESSION_NOT_FOUND" }, response: { status: 404 } }
			: { data: sessionQuestionFixture({ state, ...(state === "answered" ? { answer: { clientAnswerId: "external-answer", questionVersion: 1, answers: [{ questionKey: "target", selectedOptionIds: ["stage"] }, { questionKey: "checks", selectedOptionIds: ["test"] }], savedAt: "2026-10-29T01:00:00Z", deliveryState: "queued" } } : {}) }), response: { status: 200 } }));
	renderHook(() => useQuestionAnswerRecovery(), { wrapper });
	await waitFor(() => expect(useQuestionDraftStore.getState().entries[a]).toBeUndefined());
	expect(localStorage.getItem(`${questionDraftStorageKey}:${a}`)).toBeNull();
	expect(useQuestionDraftStore.getState().entries[b].fields.target.selectedOptionIds).toEqual(["prod"]);
	expect(get).toHaveBeenCalledTimes(2);
	expect(post).not.toHaveBeenCalled();
});

it("synchronizes another window's entry without replacing unrelated drafts", async () => {
	const { key } = ready();
	get.mockRejectedValue(new Error("offline"));
	renderHook(() => useQuestionAnswerRecovery(), { wrapper });
	const externalKey = JSON.stringify([null, "session", "ask-other", 1]);
	const value = JSON.stringify({ scope: { sessionId: "session", questionId: "ask-other", version: 1 }, question: sessionQuestionFixture({ id: "ask-other" }), fields: { target: { selectedOptionIds: ["prod"], otherSelected: false, otherText: "" }, checks: { selectedOptionIds: [], otherSelected: false, otherText: "" } }, phase: { state: "editing" }, updatedAt: Date.now() });
	act(() => window.dispatchEvent(Object.assign(new Event("storage"), { key: `${questionDraftStorageKey}:${externalKey}`, newValue: value, storageArea: localStorage })));
	await waitFor(() => expect(useQuestionDraftStore.getState().entries[externalKey]?.fields.target.selectedOptionIds).toEqual(["prod"]));
	expect(useQuestionDraftStore.getState().entries[key].phase.state).toBe("submitting");
	act(() => window.dispatchEvent(Object.assign(new Event("storage"), { key: `${questionDraftStorageKey}:${externalKey}`, newValue: null, storageArea: localStorage })));
	expect(useQuestionDraftStore.getState().entries[externalKey].fields.target.selectedOptionIds).toEqual(["prod"]);
	expect(useQuestionDraftStore.getState().storageAvailable).toBe(false);
	expect(useQuestionDraftStore.getState().entries[key]).toBeDefined();
});

it("shares one source GET across revised drafts and the visible source observer", async () => {
	const a = useQuestionDraftStore.getState().ensure(sessionQuestionFixture());
	useQuestionDraftStore.getState().setField(a, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "" });
	useQuestionDraftStore.getState().ensure(sessionQuestionFixture({ version: 2 }));
	get.mockResolvedValue({ data: sessionQuestionFixture({ version: 2 }), response: { status: 200 } });
	await client.fetchQuery(questionSourceQueryOptions({ sessionId: "session", questionId: "ask-1" }));
	renderHook(() => useQuestionAnswerRecovery(), { wrapper });
	await waitFor(() => expect(useQuestionDraftStore.getState().entries[a]?.phase.state).toBe("conflict"));
	expect(get).toHaveBeenCalledTimes(1);
});
