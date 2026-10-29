import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { largeSessionQuestionFixture, sessionQuestionFixture } from "../test/fixtures/session-question";
import { parseSessionQuestion } from "../lib/session-questions";
import { createQuestionDraftStore, questionDraftKey, questionDraftStorageKey } from "./question-draft-store";

beforeEach(() => localStorage.clear());
afterEach(() => vi.useRealTimers());

function ready(store: ReturnType<typeof createQuestionDraftStore>, hostId?: string) {
	const key = store.getState().ensure(sessionQuestionFixture(), hostId);
	store.getState().setField(key, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "kept but not submitted" });
	store.getState().setField(key, "checks", { selectedOptionIds: ["test"], otherSelected: false, otherText: "" });
	return key;
}

describe("versioned question drafts", () => {
	it("does not persist blank visits, and removes an edited draft closed by another client", () => {
		const store = createQuestionDraftStore(localStorage);
		store.getState().ensure(sessionQuestionFixture());
		expect(localStorage.length).toBe(0);
		const key = ready(store);
		store.getState().reconcileSource({ sessionId: "session", questionId: "ask-1" }, sessionQuestionFixture({ state: "cancelled" }));
		expect(store.getState().entries[key]).toBeUndefined();
		expect(createQuestionDraftStore(localStorage).getState().entries[key]).toBeUndefined();
	});

	it("keeps only the newest meaningful old version for reference, without touching another host", () => {
		const store = createQuestionDraftStore(localStorage); const oldest = ready(store); const remote = ready(store, "remote");
		const old = store.getState().ensure(sessionQuestionFixture({ version: 2 }));
		store.getState().setField(old, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "" });
		const current = store.getState().ensure(sessionQuestionFixture({ version: 3 }));
		store.getState().reconcileSource({ sessionId: "session", questionId: "ask-1" }, sessionQuestionFixture({ version: 3 }));
		expect(store.getState().entries[oldest]).toBeUndefined();
		expect(store.getState().entries[old]).toMatchObject({ phase: { state: "conflict" }, fields: { target: { selectedOptionIds: ["prod"] } } });
		expect(store.getState().entries[current].fields.target.selectedOptionIds).toEqual([]);
		expect(store.getState().entries[remote]).toBeDefined();
	});

	it("bounds new saved drafts without expiring open edits or unconfirmed submissions", () => {
		vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-29T00:00:00Z"));
		const store = createQuestionDraftStore(localStorage); const pending = ready(store);
		store.getState().begin(pending, "answer-1", Date.now());
		for (let i = 0; i < 40; i++) {
			const key = store.getState().ensure(sessionQuestionFixture({ id: `ask-${i + 2}` }));
			store.getState().setField(key, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "" });
		}
		expect(localStorage.length).toBeLessThanOrEqual(32);
		vi.advanceTimersByTime(8 * 24 * 60 * 60 * 1000);
		const loaded = createQuestionDraftStore(localStorage);
		expect(Object.keys(loaded.getState().entries)).toHaveLength(32);
		expect(loaded.getState().entries[pending].phase.state).toBe("unknown");
	});

	it("refuses the 33rd saved draft instead of deleting another window's open editing fields", () => {
		const a = createQuestionDraftStore(localStorage), b = createQuestionDraftStore(localStorage);
		const active = ready(a);
		a.getState().setField(active, "checks", { selectedOptionIds: ["test"], otherSelected: true, otherText: "User is still typing in window A" });
		const saved = localStorage.getItem(`${questionDraftStorageKey}:${active}`);
		for (let i = 0; i < 32; i++) {
			const key = b.getState().ensure(sessionQuestionFixture({ id: `other-${i}` }));
			b.getState().setField(key, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "new draft" });
		}
		expect(localStorage.getItem(`${questionDraftStorageKey}:${active}`)).toBe(saved);
		a.getState().syncStorage(`${questionDraftStorageKey}:${active}`, localStorage.getItem(`${questionDraftStorageKey}:${active}`));
		expect(a.getState().entries[active].fields.target.selectedOptionIds).toEqual(["stage"]);
		expect(a.getState().entries[active].fields.checks.otherText).toBe("User is still typing in window A");
		expect(b.getState().storageAvailable).toBe(false);
		expect(localStorage.length).toBe(32);
	});

	it("can answer an already saved question to free capacity without losing refused edits", () => {
		const store = createQuestionDraftStore(localStorage); const first = ready(store);
		for (let i = 0; i < 31; i++) {
			const key = store.getState().ensure(sessionQuestionFixture({ id: `saved-${i}` }));
			store.getState().setField(key, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "" });
		}
		const extra = store.getState().ensure(sessionQuestionFixture({ id: "refused" }));
		store.getState().setField(extra, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "still kept in memory" });
		expect(store.getState().storageAvailable).toBe(false);
		expect(store.getState().begin(first, "saved-answer", Date.now())).toBeDefined();
		expect(store.getState().storageAvailable).toBe(false);
		store.getState().clearSaved(first, "saved-answer");
		expect(store.getState().entries[first]).toBeUndefined();
		expect(localStorage.getItem(`${questionDraftStorageKey}:${first}`)).toBeNull();
		expect(store.getState().entries[extra].fields.target.otherText).toBe("still kept in memory");
		expect(store.getState().storageAvailable).toBe(false);
		store.getState().setField(extra, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "save after space freed" });
		expect(store.getState().storageAvailable).toBe(true);
		expect(createQuestionDraftStore(localStorage).getState().entries[extra].fields.target.otherText).toBe("save after space freed");
	});

	it("keeps old memory and disk on QuotaExceeded, then writes successfully before removing a reference", () => {
		let fail = false;
		const operations: string[] = [];
		const storage: Storage = {
			get length() { return localStorage.length; }, key: (index) => localStorage.key(index), getItem: (key) => localStorage.getItem(key), clear: () => localStorage.clear(),
			removeItem: (key) => { operations.push(`remove:${key}`); localStorage.removeItem(key); },
			setItem: (key, value) => { operations.push(`write:${key}`); if (fail) throw new DOMException("Quota exceeded", "QuotaExceededError"); localStorage.setItem(key, value); },
		};
		const store = createQuestionDraftStore(storage); const old = ready(store);
		store.getState().reconcileSource({ sessionId: "session", questionId: "ask-1" }, sessionQuestionFixture({ version: 2 }));
		for (let i = 0; i < 31; i++) {
			const key = store.getState().ensure(sessionQuestionFixture({ id: `occupant-${i}` }));
			store.getState().setField(key, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "" });
		}
		const before = localStorage.getItem(`${questionDraftStorageKey}:${old}`);
		const next = store.getState().ensure(sessionQuestionFixture({ id: "new-draft" }));
		operations.length = 0; fail = true;
		const field = { selectedOptionIds: ["stage"], otherSelected: false, otherText: "keep this new edit in memory too" };
		store.getState().setField(next, "target", field);
		expect(operations).toContain(`write:${questionDraftStorageKey}:${next}`);
		expect(operations.some((value) => value.startsWith("remove:"))).toBe(false);
		expect(localStorage.getItem(`${questionDraftStorageKey}:${old}`)).toBe(before);
		expect(store.getState().entries[old].fields.target.otherText).toBe("kept but not submitted");
		expect(store.getState().entries[next].fields.target).toEqual(field);
		expect(store.getState().storageAvailable).toBe(false);
		operations.length = 0; fail = false;
		store.getState().setField(next, "target", field);
		expect(store.getState().storageAvailable).toBe(true);
		expect(operations.indexOf(`write:${questionDraftStorageKey}:${next}`)).toBeLessThan(operations.indexOf(`remove:${questionDraftStorageKey}:${old}`));
		expect(localStorage.getItem(`${questionDraftStorageKey}:${old}`)).toBeNull();
	});

	it("expires old references while retaining an open editing draft after seven days", () => {
		vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-29T00:00:00Z"));
		const store = createQuestionDraftStore(localStorage); const old = ready(store);
		store.getState().reconcileSource({ sessionId: "session", questionId: "ask-1" }, sessionQuestionFixture({ version: 2 }));
		const current = store.getState().ensure(sessionQuestionFixture({ id: "still-open" }));
		store.getState().setField(current, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "" });
		vi.advanceTimersByTime(8 * 24 * 60 * 60 * 1000);
		store.getState().prune();
		expect(store.getState().entries[old]).toBeUndefined();
		expect(createQuestionDraftStore(localStorage).getState().entries[current]?.fields.target.selectedOptionIds).toEqual(["prod"]);
	});

	it("persists only the changed question and does not overwrite another window's question", () => {
		const writes = vi.fn((key: string, value: string) => localStorage.setItem(key, value));
		const storage: Storage = {
			get length() { return localStorage.length; }, key: (index) => localStorage.key(index),
			getItem: (key) => localStorage.getItem(key), removeItem: (key) => localStorage.removeItem(key), clear: () => localStorage.clear(), setItem: writes,
		};
		const a = createQuestionDraftStore(storage), b = createQuestionDraftStore(storage);
		const aKey = ready(a);
		const bKey = b.getState().ensure(sessionQuestionFixture({ id: "ask-other" }));
		b.getState().setField(bKey, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "" });
		writes.mockClear();
		const stringify = vi.spyOn(JSON, "stringify");
		a.getState().setField(aKey, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "typing" });
		expect(stringify.mock.calls.some(([value]) => value === a.getState().entries[aKey].question)).toBe(false);
		stringify.mockRestore();
		expect(writes).toHaveBeenCalledTimes(1);
		expect(writes).toHaveBeenCalledWith(`${questionDraftStorageKey}:${aKey}`, expect.any(String));
		const loaded = createQuestionDraftStore(localStorage);
		expect(loaded.getState().entries[bKey].fields.target.selectedOptionIds).toEqual(["prod"]);
	});

	it("bounds serialized size even when each question has a large payload", () => {
		const store = createQuestionDraftStore(localStorage);
		for (let i = 0; i < 20; i++) {
			const question = sessionQuestionFixture({ id: `large-${i}` });
			question.payload.questions[0].options[0].preview = { format: "text", content: "界".repeat(8192) };
			question.payload.questions[1].options[0].preview = { format: "text", content: "界".repeat(8192) };
			question.payload.explanation = "界".repeat(8000);
			const key = store.getState().ensure(question);
			store.getState().setField(key, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "" });
		}
		let size = 0;
		for (let i = 0; i < localStorage.length; i++) size += localStorage.getItem(localStorage.key(i) ?? "")?.length ?? 0;
		expect(size).toBeLessThanOrEqual(512 * 1024);
		expect(Object.values(createQuestionDraftStore(localStorage).getState().entries).at(0)?.question.payload.title).toBe("Choose a deployment target");
	});

	it("submits in memory at capacity without evicting unconfirmed receipts", () => {
		const store = createQuestionDraftStore(localStorage);
		for (let i = 0; i < 32; i++) {
			const key = store.getState().ensure(sessionQuestionFixture({ id: `pending-${i}` }));
			store.getState().setField(key, "target", { selectedOptionIds: ["stage"], otherSelected: false, otherText: "" });
			store.getState().setField(key, "checks", { selectedOptionIds: ["test"], otherSelected: false, otherText: "" });
			expect(store.getState().begin(key, `answer-${i}`, Date.now())).toBeDefined();
		}
		const next = ready(store);
		expect(store.getState().begin(next, "overflow", Date.now())?.clientAnswerId).toBe("overflow");
		expect(store.getState().storageAvailable).toBe(false);
		expect(store.getState().entries[next].phase.state).toBe("submitting");
		const restored = Object.values(createQuestionDraftStore(localStorage).getState().entries);
		expect(restored).toHaveLength(32);
		expect(restored.every((entry) => entry.phase.state === "unknown")).toBe(true);
	});

	it("recovers persistence without reading or modifying unpublished legacy data", () => {
		const key = ready(createQuestionDraftStore(localStorage));
		localStorage.setItem("ao.question.drafts.v1", "unpublished data");
		let unavailable = false;
		const legacyAccess: string[] = [];
		const storage: Storage = {
			get length() { return localStorage.length; }, key: (index) => localStorage.key(index),
			getItem: (key) => { if (key === "ao.question.drafts.v1") legacyAccess.push("get"); return localStorage.getItem(key); },
			removeItem: (key) => { if (key === "ao.question.drafts.v1") legacyAccess.push("remove"); localStorage.removeItem(key); }, clear: () => localStorage.clear(),
			setItem: (key, value) => { if (key === "ao.question.drafts.v1") legacyAccess.push("set"); if (unavailable) throw new DOMException("Quota exceeded", "QuotaExceededError"); localStorage.setItem(key, value); },
		};
		const store = createQuestionDraftStore(storage);
		expect(store.getState().entries[key].fields.target.otherText).toBe("kept but not submitted");
		unavailable = true;
		store.getState().setField(key, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "new edit" });
		expect(store.getState().storageAvailable).toBe(false);
		unavailable = false;
		store.getState().setField(key, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "persist again" });
		expect(store.getState().storageAvailable).toBe(true);
		expect(createQuestionDraftStore(storage).getState().entries[key].fields.target.otherText).toBe("persist again");
		expect(legacyAccess).toEqual([]);
		expect(localStorage.getItem("ao.question.drafts.v1")).toBe("unpublished data");
	});

	it("submits an already saved draft when its larger pending snapshot hits quota", () => {
		const storage: Storage = {
			get length() { return localStorage.length; }, key: (index) => localStorage.key(index), getItem: (key) => localStorage.getItem(key),
			removeItem: (key) => localStorage.removeItem(key), clear: () => localStorage.clear(),
			setItem: (key, value) => { if (JSON.parse(value).phase.state === "submitting") throw new DOMException("Quota exceeded", "QuotaExceededError"); localStorage.setItem(key, value); },
		};
		const store = createQuestionDraftStore(storage); const key = ready(store);
		const before = localStorage.getItem(`${questionDraftStorageKey}:${key}`);
		expect(store.getState().begin(key, "quota-answer", Date.now())?.clientAnswerId).toBe("quota-answer");
		expect(store.getState().entries[key].phase.state).toBe("submitting");
		expect(store.getState().storageAvailable).toBe(false);
		expect(localStorage.getItem(`${questionDraftStorageKey}:${key}`)).toBe(before);
	});

	it("submits a valid frozen answer when four saved large drafts occupy 516040 characters", () => {
		const store = createQuestionDraftStore(localStorage); let first = "";
		for (let i = 0; i < 4; i++) {
			const question = largeSessionQuestionFixture({ id: `large-${i}`, clientKey: `large-${i}`, sequence: i + 1 });
			parseSessionQuestion(question, "session");
			expect(new TextEncoder().encode(JSON.stringify(question.payload)).length).toBe(112313);
			const key = store.getState().ensure(question); if (i === 0) first = key;
			for (let q = 0; q < 4; q++) store.getState().setField(key, `q${q}`, { selectedOptionIds: [], otherSelected: true, otherText: "o".repeat(4000) });
		}
		const stored = () => {
			const values: Record<string, string> = {};
			for (let i = 0; i < localStorage.length; i++) {
				const key = localStorage.key(i);
				if (key?.startsWith(`${questionDraftStorageKey}:`)) values[key] = localStorage.getItem(key) ?? "";
			}
			return values;
		};
		const before = stored();
		expect(Object.keys(before)).toHaveLength(4);
		expect(Object.values(before).reduce((size, value) => size + (value?.length ?? 0), 0)).toBe(516040);
		expect(store.getState().storageAvailable).toBe(true);
		const answer = store.getState().begin(first, "existing-at-size-limit", Date.now());
		expect(answer).toBeDefined();
		expect(new TextEncoder().encode(JSON.stringify(answer)).length).toBe(16334);
		expect(store.getState().entries[first].phase.state).toBe("submitting");
		expect(store.getState().storageAvailable).toBe(false);
		expect(stored()).toEqual(before);
	});

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
		const saved = JSON.parse(localStorage.getItem(`${questionDraftStorageKey}:${key}`) ?? "null");
		saved.phase.answer.questionId = "foreign";
		localStorage.setItem(`${questionDraftStorageKey}:${key}`, JSON.stringify(saved));
		expect(createQuestionDraftStore(localStorage).getState().entries[key]).toBeUndefined();
	});
});

it("does not notify subscribers or inspect storage when a prune changes nothing", () => {
	const store = createQuestionDraftStore(localStorage);
	ready(store);
	const previous = store.getState();
	const listener = vi.fn();
	const unsubscribe = store.subscribe(listener);
	const read = vi.spyOn(localStorage, "getItem");
	store.getState().prune();
	expect(store.getState()).toBe(previous);
	expect(listener).not.toHaveBeenCalled();
	expect(read).not.toHaveBeenCalled();
	unsubscribe(); read.mockRestore();
});

it("expires long-unconfirmed inactive edits while protecting an editor in another window", () => {
	vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-01T00:00:00Z"));
	const first = createQuestionDraftStore(localStorage);
	const hidden = ready(first, "removed-host");
	const active = first.getState().ensure(sessionQuestionFixture({ id: "visible" }), "removed-host");
	first.getState().setField(active, "target", { selectedOptionIds: [], otherSelected: true, otherText: "Still editing in another window" });
	vi.setSystemTime(new Date("2026-11-02T00:00:00Z"));
	const editor = createQuestionDraftStore(localStorage);
	const release = editor.getState().retainVisible(active);
	first.getState().prune();
	expect(createQuestionDraftStore(localStorage).getState().entries[hidden]).toBeUndefined();
	expect(createQuestionDraftStore(localStorage).getState().entries[active].fields.target.otherText).toBe("Still editing in another window");
	release(); first.getState().prune();
	expect(createQuestionDraftStore(localStorage).getState().entries[active]).toBeUndefined();
});
