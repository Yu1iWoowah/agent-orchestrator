import { create } from "zustand";
import type { components } from "../../api/schema";
import type { QuestionFields, QuestionField, SessionQuestion } from "../lib/session-questions";
import { emptyQuestionFields, parseSessionQuestion, parseQuestionSubmission, questionAnswers } from "../lib/session-questions";

export interface QuestionScope { hostId?: string; sessionId: string; questionId: string; version: number }
type Submission = components["schemas"]["AttentionAnswerSubmission"];
export type QuestionDraftPhase =
	| { state: "editing"; message?: string; fieldErrors?: Record<string, string>; errorCode?: "answerTooLarge" }
	| { state: "submitting"; answer: Submission; startedAt: number }
	| { state: "unknown"; answer: Submission; nextRetryAt: number; message?: string }
	| { state: "conflict"; message: string };
export interface QuestionDraft { scope: QuestionScope; question: SessionQuestion; fields: QuestionFields; phase: QuestionDraftPhase; updatedAt: number }
type SourceScope = Omit<QuestionScope, "version">;
interface QuestionDraftState {
	entries: Record<string, QuestionDraft>;
	storageAvailable: boolean;
	ensure: (question: SessionQuestion, hostId?: string) => string;
	setField: (key: string, promptKey: string, field: QuestionField) => void;
	begin: (key: string, clientAnswerId: string, at: number) => Submission | undefined;
	retry: (key: string, at: number) => Submission | undefined;
	unknown: (key: string, clientAnswerId: string, message: string, at: number) => void;
	conflict: (key: string, clientAnswerId: string, message: string) => void;
	rejected: (key: string, clientAnswerId: string, message: string, fieldErrors?: Record<string, string>) => void;
	clearSaved: (key: string, clientAnswerId: string) => void;
	reconcileSource: (scope: SourceScope, question: SessionQuestion | null) => void;
	prune: () => void;
	syncStorage: (key: string | null, value: string | null) => void;
}

export const questionDraftStorageKey = "ao.question.drafts.v2";
const maxDrafts = 32;
const maxStorageChars = 512 * 1024;
const retention = 7 * 24 * 60 * 60 * 1000;
const storageKey = (key: string) => `${questionDraftStorageKey}:${key}`;
const hasFields = (entry: QuestionDraft) => Object.values(entry.fields).some((field) => field.selectedOptionIds.length || field.otherSelected || field.otherText);
const isPending = (entry: QuestionDraft) => entry.phase.state === "submitting" || entry.phase.state === "unknown";
const protectsInput = (entry: QuestionDraft) => isPending(entry) || entry.phase.state === "editing" && hasFields(entry);
const sameSource = (a: SourceScope, b: SourceScope) => (a.hostId ?? null) === (b.hostId ?? null) && a.sessionId === b.sessionId && a.questionId === b.questionId;
export const questionDraftKey = (scope: QuestionScope) => JSON.stringify([scope.hostId ?? null, scope.sessionId, scope.questionId, scope.version]);
const isRecord = (value: unknown): value is Record<string, unknown> =>
	typeof value === "object" && value !== null && !Array.isArray(value);

function savedFields(value: unknown, question: SessionQuestion): QuestionFields {
	if (!isRecord(value)) throw new Error("Invalid fields");
	const fields: QuestionFields = {};
	for (const prompt of question.payload.questions) {
		const field = value[prompt.key];
		if (!isRecord(field) || !Array.isArray(field.selectedOptionIds) ||
			!field.selectedOptionIds.every((id: unknown) => typeof id === "string") ||
			typeof field.otherSelected !== "boolean" || typeof field.otherText !== "string") throw new Error("Invalid field");
		fields[prompt.key] = { selectedOptionIds: field.selectedOptionIds, otherSelected: field.otherSelected, otherText: field.otherText };
	}
	return fields;
}

function parseDraft(key: string, value: unknown): QuestionDraft | undefined {
	try {
		if (!isRecord(value) || !isRecord(value.scope) || typeof value.scope.sessionId !== "string" ||
			(value.scope.hostId !== undefined && typeof value.scope.hostId !== "string") || !isRecord(value.phase)) return;
		const updatedAt = typeof value.updatedAt === "number" && Number.isFinite(value.updatedAt) ? Math.min(value.updatedAt, Date.now()) : Date.now();
		if (value.phase.state === "conflict" && Date.now() - updatedAt >= retention) return;
		const question = parseSessionQuestion(value.question, value.scope.sessionId);
		if (question.state !== "open") return;
		const scope: QuestionScope = { hostId: value.scope.hostId, sessionId: question.originSessionId, questionId: question.id, version: question.version };
		if (value.scope.questionId !== question.id || value.scope.version !== question.version || questionDraftKey(scope) !== key) return;
		const fields = savedFields(value.fields, question);
		let phase: QuestionDraftPhase;
		switch (value.phase.state) {
			case "editing": phase = { state: "editing" }; break;
			case "conflict": phase = { state: "conflict", message: typeof value.phase.message === "string" ? value.phase.message : "" }; break;
			case "submitting":
			case "unknown": phase = { state: "unknown", answer: parseQuestionSubmission(value.phase.answer, question), nextRetryAt: 0 }; break;
			default: return;
		}
		const entry = { scope, question, fields, phase, updatedAt };
		return hasFields(entry) || isPending(entry) ? entry : undefined;
	} catch { /* Invalid saved input never becomes an automatic answer. */ }
	return undefined;
}

function diskEntries(storage: Storage): Map<string, string> {
	const result = new Map<string, string>();
	for (let i = 0; i < storage.length; i++) {
		const key = storage.key(i);
		if (key?.startsWith(`${questionDraftStorageKey}:`)) result.set(key.slice(questionDraftStorageKey.length + 1), storage.getItem(key) ?? "null");
	}
	return result;
}

function readDrafts(storage: Storage): Record<string, QuestionDraft> {
	const entries: Record<string, QuestionDraft> = {};
	try {
		const candidates = diskEntries(storage);
		let count = 0, size = 0;
		for (const [key, raw] of [...candidates].reverse()) {
			try {
				const value: unknown = JSON.parse(raw);
				const reference = isRecord(value) && isRecord(value.phase) && value.phase.state === "conflict";
				// Only references may be discarded at restore; existing open edits remain protected.
				if (reference && (count >= maxDrafts || size + raw.length > maxStorageChars)) { storage.removeItem(storageKey(key)); continue; }
				const entry = parseDraft(key, value);
				if (entry) { entries[key] = entry; count++; size += raw.length; }
				else storage.removeItem(storageKey(key));
			} catch { storage.removeItem(storageKey(key)); }
		}
	} catch { /* Storage may be unavailable. */ }
	return entries;
}

function pendingAnswer(entry: QuestionDraft | undefined, clientAnswerId: string) {
	return entry && (entry.phase.state === "submitting" || entry.phase.state === "unknown") &&
		entry.phase.answer.clientAnswerId === clientAnswerId;
}

export function createQuestionDraftStore(storage: Storage) {
	return create<QuestionDraftState>((set, get) => {
		const unsaved = new Set<string>();
		const snapshots = new WeakMap<SessionQuestion, string>();
		const metadata = new Map<string, { raw: string; reference: boolean; at: number }>();
		const encode = (entry: QuestionDraft) => {
			const { question, ...mutable } = entry;
			let snapshot = snapshots.get(question);
			if (!snapshot) { snapshot = JSON.stringify(question); snapshots.set(question, snapshot); }
			return `${JSON.stringify(mutable).slice(0, -1)},"question":${snapshot}}`;
		};
		const persist = (entries: Record<string, QuestionDraft>, previous: Record<string, QuestionDraft>, protectedKey?: string) => {
			const next = { ...entries };
			const changed = Object.keys(entries).filter((key) => entries[key] !== previous[key]);
			const failed = (restoreRemoved: boolean) => {
				for (const key of changed) if (hasFields(entries[key]) || isPending(entries[key])) unsaved.add(key);
				return { entries: restoreRemoved ? { ...previous, ...entries } : entries, storageAvailable: false };
			};
			try {
				const before = diskEntries(storage), after = new Map(before);
				for (const key of Object.keys(previous)) if (!next[key]) after.delete(key);
				for (const [key, entry] of Object.entries(next)) {
					if (entry.phase.state === "conflict" && Date.now() - entry.updatedAt >= retention) { delete next[key]; after.delete(key); }
					else if (!hasFields(entry) && !isPending(entry)) after.delete(key);
					else if (entry !== previous[key]) after.set(key, encode(entry));
				}
				let size = [...after.values()].reduce((total, raw) => total + raw.length, 0);
				const removable = [...after].flatMap(([key, raw]) => {
					let meta = metadata.get(key);
					if (meta?.raw !== raw) {
						let value: unknown;
						try { value = JSON.parse(raw); } catch {
							if (next[key] && protectsInput(next[key])) unsaved.add(key);
							else { after.delete(key); delete next[key]; size -= raw.length; }
							return [];
						}
						meta = { raw, reference: isRecord(value) && isRecord(value.phase) && value.phase.state === "conflict", at: isRecord(value) && typeof value.updatedAt === "number" ? value.updatedAt : 0 };
						metadata.set(key, meta);
					}
					return key !== protectedKey && meta.reference && !(next[key] && protectsInput(next[key])) ? [{ key, size: raw.length, at: meta.at }] : [];
				}).sort((a, b) => a.at - b.at);
				for (const item of removable) {
					if (after.size <= maxDrafts && size <= maxStorageChars) break;
					after.delete(item.key); delete next[item.key]; size -= item.size;
				}
				if (after.size > maxDrafts || size > maxStorageChars) {
					const beforeSize = [...before.values()].reduce((total, raw) => total + raw.length, 0);
					// Existing oversized storage may shrink without admitting more persisted data.
					if (after.size > before.size || protectedKey && size > beforeSize) {
						return failed(false);
					}
				}
				const writes = [...after].filter(([key, raw]) => raw !== before.get(key));
				// Persist the current submission last; no destructive cleanup precedes successful writes.
				for (const [key, raw] of writes.filter(([key]) => key !== protectedKey)) storage.setItem(storageKey(key), raw);
				for (const [key, raw] of writes.filter(([key]) => key === protectedKey)) storage.setItem(storageKey(key), raw);
				for (const key of before.keys()) if (!after.has(key)) storage.removeItem(storageKey(key));
				for (const key of metadata.keys()) if (!after.has(key)) metadata.delete(key);
				for (const key of changed) if (after.has(key)) unsaved.delete(key);
				for (const key of unsaved) if (!next[key] || !hasFields(next[key]) && !isPending(next[key])) unsaved.delete(key);
				return { entries: next, storageAvailable: unsaved.size === 0 };
			} catch { return failed(true); }
		};
		const update = (change: (entries: Record<string, QuestionDraft>) => Record<string, QuestionDraft>, protectedKey?: string) =>
			set((state) => {
				const entries = change(state.entries);
				if (entries === state.entries) return state;
				return persist(entries, state.entries, protectedKey);
			});
		const change = (key: string, transform: (entry: QuestionDraft) => QuestionDraft) =>
			update((entries) => {
				const entry = entries[key];
				if (!entry) return entries;
				const next = transform(entry);
				return next === entry ? entries : { ...entries, [key]: { ...next, updatedAt: Date.now() } };
			}, key);
		const initial = persist(readDrafts(storage), {});
		return {
			...initial,
			ensure: (question, hostId) => {
				const scope = { hostId, sessionId: question.originSessionId, questionId: question.id, version: question.version };
				const key = questionDraftKey(scope);
				if (!get().entries[key]) update((entries) => ({ ...Object.fromEntries(Object.entries(entries).filter(([, entry]) => hasFields(entry) || isPending(entry))), [key]: { scope, question: structuredClone(question), fields: emptyQuestionFields(question.payload), phase: { state: "editing" }, updatedAt: Date.now() } }), key);
				return key;
			},
			setField: (key, promptKey, field) => change(key, (entry) =>
				entry.phase.state !== "editing" ? entry : { ...entry,
					fields: { ...entry.fields, [promptKey]: { ...field, selectedOptionIds: [...field.selectedOptionIds] } }, phase: { state: "editing" },
				}),
			begin: (key, clientAnswerId, at) => {
				const entry = get().entries[key];
				if (!entry || entry.phase.state !== "editing" || !/^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/.test(clientAnswerId)) return;
				const built = questionAnswers(entry.question.payload, entry.fields);
				if (Object.keys(built.errors).length) return;
				const answer = { questionId: entry.scope.questionId, expectedVersion: entry.scope.version, clientAnswerId, answers: built.answers };
				if (new TextEncoder().encode(JSON.stringify(answer)).length > 32 * 1024) {
					change(key, (current) => ({ ...current, phase: { state: "editing", errorCode: "answerTooLarge" } })); return;
				}
				change(key, (current) => ({ ...current, phase: { state: "submitting", answer, startedAt: at } }));
				// Storage is best-effort: retain its warning, but let the server save the answer.
				// This window keeps the immutable ID for retries even when it cannot survive a restart.
				return structuredClone(answer);
			},
			retry: (key, at) => {
				const entry = get().entries[key];
				if (!entry || entry.phase.state !== "unknown" || at < entry.phase.nextRetryAt) return;
				const answer = entry.phase.answer;
				change(key, (current) => ({ ...current, phase: { state: "submitting", answer, startedAt: at } }));
				return structuredClone(answer);
			},
			unknown: (key, clientAnswerId, message, at) => change(key, (entry) =>
				pendingAnswer(entry, clientAnswerId) && (entry.phase.state === "submitting" || entry.phase.state === "unknown")
					? { ...entry, phase: { state: "unknown", answer: entry.phase.answer, nextRetryAt: at + 2000, message } } : entry),
			conflict: (key, clientAnswerId, message) => change(key, (entry) =>
				pendingAnswer(entry, clientAnswerId) ? { ...entry, phase: { state: "conflict", message } } : entry),
			rejected: (key, clientAnswerId, message, fieldErrors) => change(key, (entry) =>
				pendingAnswer(entry, clientAnswerId) ? { ...entry, phase: { state: "editing", message, fieldErrors } } : entry),
			clearSaved: (key, clientAnswerId) => update((entries) => {
				if (!pendingAnswer(entries[key], clientAnswerId)) return entries;
				const next = { ...entries }; delete next[key]; return next;
			}),
			reconcileSource: (scope, question) => update((entries) => {
				const matching = Object.entries(entries).filter(([, entry]) => sameSource(entry.scope, scope));
				const reference = matching.filter(([, entry]) => hasFields(entry) && (question?.state === "open" && entry.scope.version < question.version || entry.phase.state === "conflict"))
					.sort(([, a], [, b]) => b.scope.version - a.scope.version)[0]?.[0];
				let next = entries;
				for (const [key, entry] of matching) {
					if (isPending(entry) || question && (entry.scope.version > question.version || question.state === "open" && entry.scope.version === question.version)) continue;
					if (key === reference && entry.phase.state === "conflict") continue;
					if (next === entries) next = { ...entries };
					if (key === reference) {
						if (entry.phase.state !== "conflict") next[key] = { ...entry, phase: { state: "conflict", message: "Question changed; previous draft retained for reference." } };
					} else delete next[key];
				}
				return next;
			}),
			prune: () => update((entries) => ({ ...entries })),
			syncStorage: (key, value) => {
				if (key !== null && !key.startsWith(`${questionDraftStorageKey}:`)) return;
				set((state) => {
					if (key === null) {
						const entries = Object.fromEntries(Object.entries(state.entries).filter(([, entry]) => protectsInput(entry)));
						for (const key of Object.keys(entries)) unsaved.add(key);
						return { entries, storageAvailable: false };
					}
					const draftKey = key.slice(questionDraftStorageKey.length + 1);
					const current = state.entries[draftKey];
					if (current && isPending(current)) {
						if (value !== null) return state;
						unsaved.add(draftKey); return { storageAvailable: false };
					}
					let entry: QuestionDraft | undefined;
					try { if (value) entry = parseDraft(draftKey, JSON.parse(value)); } catch { /* Ignore invalid external storage. */ }
					if (!entry && current && protectsInput(current)) { unsaved.add(draftKey); return { storageAvailable: false }; }
					if (entry && state.entries[draftKey] && entry.updatedAt < state.entries[draftKey].updatedAt) return state;
					const entries = { ...state.entries };
					if (entry) entries[draftKey] = entry; else delete entries[draftKey];
					unsaved.delete(draftKey);
					return { entries, storageAvailable: unsaved.size === 0 };
				});
			},
		};
	});
}
export const useQuestionDraftStore = createQuestionDraftStore(window.localStorage);
