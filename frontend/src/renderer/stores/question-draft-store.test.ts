import { beforeEach, describe, expect, it } from "vitest";
import { sessionQuestionFixture } from "../test/fixtures/session-question";
import { createQuestionDraftStore, questionDraftKey, questionDraftStorageKey } from "./question-draft-store";

beforeEach(() => localStorage.clear());

function ready(store: ReturnType<typeof createQuestionDraftStore>, hostId?: string) {
	const key = store.getState().ensure(sessionQuestionFixture(), hostId);
	store.getState().setField(key, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "kept but not submitted" });
	store.getState().setField(key, "checks", { selectedOptionIds: ["test"], otherSelected: false, otherText: "" });
	return key;
}

describe("versioned question drafts", () => {
	it("isolates host, session, question and version, with zero preselection", () => {
		const store = createQuestionDraftStore(localStorage); const key = ready(store);
		for (const [question, host] of [
			[sessionQuestionFixture({ version: 2 }), undefined],
			[sessionQuestionFixture({ originSessionId: "other" }), undefined],
			[sessionQuestionFixture({ id: "ask-other" }), undefined],
			[sessionQuestionFixture(), "remote"],
		] as const) {
			const other = store.getState().ensure(question, host);
			expect(other).not.toBe(key);
			expect(store.getState().entries[other].fields.target.selectedOptionIds).toEqual([]);
		}
		expect(store.getState().entries[key].fields.target.selectedOptionIds).toEqual(["stage"]);
		expect(key).toBe(questionDraftKey({ sessionId: "session", questionId: "ask-1", version: 1 }));
	});

	it("keeps drafts after a new store opens and preserves unselected Other", () => {
		const first = createQuestionDraftStore(localStorage); const key = ready(first);
		const reloaded = createQuestionDraftStore(localStorage);
		expect(reloaded.getState().entries[key].fields.target.otherText).toBe("kept but not submitted");
		expect(reloaded.getState().entries[key].phase.state).toBe("editing");
	});

	it("freezes one submitted snapshot and rejects a double submit or edits while pending", () => {
		const store = createQuestionDraftStore(localStorage); const key = ready(store);
		const submission = store.getState().begin(key, "answer-1", 10);
		expect(submission?.clientAnswerId).toBe("answer-1");
		expect(submission?.answers[0]).not.toHaveProperty("otherText");
		expect(store.getState().begin(key, "answer-2", 11)).toBeUndefined();
		store.getState().setField(key, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "" });
		expect(store.getState().entries[key].fields.target.selectedOptionIds).toEqual(["stage"]);
		if (!submission) throw new Error("missing submission");
		submission.answers[0].selectedOptionIds.push("prod");
		const phase = store.getState().entries[key].phase;
		if (phase.state !== "submitting") throw new Error("wrong phase");
		expect(phase.answer.answers[0].selectedOptionIds).toEqual(["stage"]);
	});

	it("restores in-flight work as unknown with the same clientAnswerId, never as cancelled", () => {
		const first = createQuestionDraftStore(localStorage); const key = ready(first);
		first.getState().begin(key, "answer-1", 10);
		const reloaded = createQuestionDraftStore(localStorage); const phase = reloaded.getState().entries[key].phase;
		expect(phase.state).toBe("unknown");
		if (phase.state !== "unknown") throw new Error("wrong phase");
		expect(phase.answer.clientAnswerId).toBe("answer-1");
		const retry = reloaded.getState().retry(key, 3000);
		expect(retry?.clientAnswerId).toBe("answer-1");
		expect(retry?.answers).toEqual(phase.answer.answers);
	});

	it("keeps a conflicted draft read-only and does not copy its consent into v2", () => {
		const store = createQuestionDraftStore(localStorage); const key = ready(store);
		store.getState().begin(key, "answer-1", 10);
		store.getState().conflict(key, "answer-1", "Question changed");
		expect(store.getState().entries[key].phase.state).toBe("conflict");
		expect(store.getState().begin(key, "new-answer", 20)).toBeUndefined();
		const next = store.getState().ensure(sessionQuestionFixture({ version: 2 }));
		expect(store.getState().entries[next].fields.target.selectedOptionIds).toEqual([]);
		expect(store.getState().entries[key].fields.target.selectedOptionIds).toEqual(["stage"]);
	});

	it("ignores a late result for a different client ID", () => {
		const store = createQuestionDraftStore(localStorage); const key = ready(store);
		store.getState().begin(key, "answer-1", 10);
		store.getState().clearSaved(key, "foreign-answer");
		expect(store.getState().entries[key]).toBeDefined();
		store.getState().clearSaved(key, "answer-1");
		expect(store.getState().entries[key]).toBeUndefined();
	});

	it("does not recover fabricated or out-of-scope submissions from malformed storage", () => {
		const store = createQuestionDraftStore(localStorage); const key = ready(store);
		store.getState().begin(key, "answer-1", 10);
		const saved = JSON.parse(localStorage.getItem(questionDraftStorageKey) ?? "null");
		saved.entries[key].phase.answer.questionId = "foreign";
		localStorage.setItem(questionDraftStorageKey, JSON.stringify(saved));
		expect(createQuestionDraftStore(localStorage).getState().entries[key]).toBeUndefined();
	});
});
