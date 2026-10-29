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
export interface QuestionDraft { scope: QuestionScope; question: SessionQuestion; fields: QuestionFields; phase: QuestionDraftPhase }
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
}

export const questionDraftStorageKey = "ao.question.drafts.v1";
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

function readDrafts(storage: Storage): Record<string, QuestionDraft> {
	try {
		const saved: unknown = JSON.parse(storage.getItem(questionDraftStorageKey) ?? "null");
		if (!isRecord(saved) || saved.version !== 1 || !isRecord(saved.entries)) return {};
		const entries: Record<string, QuestionDraft> = {};
		for (const [key, value] of Object.entries(saved.entries)) {
			try {
				if (!isRecord(value) || !isRecord(value.scope) || typeof value.scope.sessionId !== "string" ||
					(value.scope.hostId !== undefined && typeof value.scope.hostId !== "string") || !isRecord(value.phase)) continue;
				const question = parseSessionQuestion(value.question, value.scope.sessionId);
				if (question.state !== "open") continue;
				const scope: QuestionScope = { hostId: value.scope.hostId, sessionId: question.originSessionId, questionId: question.id, version: question.version };
				if (value.scope.questionId !== question.id || value.scope.version !== question.version || questionDraftKey(scope) !== key) continue;
				const fields = savedFields(value.fields, question);
				let phase: QuestionDraftPhase;
				switch (value.phase.state) {
					case "editing": phase = { state: "editing" }; break;
					case "conflict": phase = { state: "conflict", message: typeof value.phase.message === "string" ? value.phase.message : "" }; break;
					case "submitting":
					case "unknown": phase = { state: "unknown", answer: parseQuestionSubmission(value.phase.answer, question), nextRetryAt: 0 }; break;
					default: continue;
				}
				entries[key] = { scope, question, fields, phase };
			} catch { /* Invalid saved input never becomes an automatic answer. */ }
		}
		return entries;
	} catch { return {}; }
}

function pendingAnswer(entry: QuestionDraft | undefined, clientAnswerId: string) {
	return entry && (entry.phase.state === "submitting" || entry.phase.state === "unknown") &&
		entry.phase.answer.clientAnswerId === clientAnswerId;
}

export function createQuestionDraftStore(storage: Storage) {
	return create<QuestionDraftState>((set, get) => {
		const update = (change: (entries: Record<string, QuestionDraft>) => Record<string, QuestionDraft>) =>
			set((state) => {
				const entries = change(state.entries);
				if (entries === state.entries) return state;
				let storageAvailable = true;
				try { storage.setItem(questionDraftStorageKey, JSON.stringify({ version: 1, entries })); }
				catch { storageAvailable = false; }
				return { entries, storageAvailable };
			});
		const change = (key: string, transform: (entry: QuestionDraft) => QuestionDraft) =>
			update((entries) => {
				const entry = entries[key];
				if (!entry) return entries;
				const next = transform(entry);
				return next === entry ? entries : { ...entries, [key]: next };
			});
		return {
			entries: readDrafts(storage), storageAvailable: true,
			ensure: (question, hostId) => {
				const scope = { hostId, sessionId: question.originSessionId, questionId: question.id, version: question.version };
				const key = questionDraftKey(scope);
				if (!get().entries[key]) update((entries) => ({ ...entries, [key]: { scope, question: structuredClone(question), fields: emptyQuestionFields(question.payload), phase: { state: "editing" } } }));
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
		};
	});
}
export const useQuestionDraftStore = createQuestionDraftStore(window.localStorage);
