import type { components } from "../../api/schema";
import { isValidDecisionPreview } from "./decision-preview";

export type SessionQuestion = components["schemas"]["QuestionResponse"];
export type QuestionPayload = components["schemas"]["AttentionQuestionPayload"];
export type QuestionAnswer = components["schemas"]["AttentionQuestionAnswer"];
export type QuestionReceipt = components["schemas"]["AttentionAnswerReceipt"];
export type QuestionPage = components["schemas"]["QuestionPageResponse"];
export interface QuestionField { selectedOptionIds: string[]; otherSelected: boolean; otherText: string; notes?: string }
export type QuestionFields = Record<string, QuestionField>;
export type QuestionFieldError = "required" | "single" | "unknown" | "otherRequired" | "otherTooLong" | "notesTooLong";

export class InvalidQuestionResponse extends Error {
	constructor() { super("Invalid structured question response."); }
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
	typeof value === "object" && value !== null && !Array.isArray(value);
const isText = (value: unknown, max: number): value is string =>
	typeof value === "string" && Boolean(value.trim()) && Array.from(value).length <= max;
const isToken = (value: unknown, max: number): value is string =>
	typeof value === "string" && value.length > 0 && value.length <= max && /^[A-Za-z0-9][A-Za-z0-9_-]*$/.test(value);
const isCount = (value: unknown): value is number =>
	typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const isVersion = (value: unknown): value is number => isCount(value) && value > 0;
const isDate = (value: unknown): value is string =>
	typeof value === "string" && Number.isFinite(Date.parse(value));
const optionalDate = (value: unknown) => value === undefined || value === null || isDate(value);
const optionalString = (value: unknown) => value === undefined || typeof value === "string";

function isOption(value: unknown): value is components["schemas"]["AttentionQuestionOption"] {
	return isRecord(value) && isToken(value.id, 64) && isText(value.label, 80) &&
		!["other", "其他"].includes(value.label.trim().toLowerCase()) &&
		isText(value.description, 1000) &&
		(value.recommended === undefined || typeof value.recommended === "boolean") &&
		(value.preview === undefined || isValidDecisionPreview(value.preview));
}

function isPrompt(value: unknown): value is components["schemas"]["AttentionPrompt"] {
	if (!isRecord(value) || !isToken(value.key, 64) || !isText(value.header, 24) ||
		!isText(value.question, 2000) ||
		(value.explanation !== undefined && !isText(value.explanation, 4000)) ||
		(value.multiSelect !== undefined && typeof value.multiSelect !== "boolean") ||
		!Array.isArray(value.options) || value.options.length < 2 || value.options.length > 4) return false;
	const options: unknown[] = value.options;
	if (!options.every(isOption)) return false;
	return new Set(options.map((option) => option.id)).size === options.length &&
		new Set(options.map((option) => option.label.trim())).size === options.length &&
		options.every((option, index) => !option.recommended || index === 0);
}

function isAuthorization(value: unknown): value is components["schemas"]["AttentionAuthorization"] {
	return isRecord(value) && isText(value.action, 1000) && isText(value.scope, 2000) &&
		isText(value.limits, 2000) && Array.isArray(value.evidence) &&
		value.evidence.length >= 1 && value.evidence.length <= 4 &&
		value.evidence.every((entry: unknown) => isText(entry, 1000));
}

function isPayload(value: unknown): value is QuestionPayload {
	if (!isRecord(value) || value.schemaVersion !== 1 || !isText(value.title, 120) ||
		!isText(value.explanation, 8000) || !Array.isArray(value.questions) ||
		value.questions.length < 1 || value.questions.length > 4) return false;
	const questions: unknown[] = value.questions;
	return questions.every(isPrompt) &&
		new Set(questions.map((question) => question.key)).size === questions.length &&
		(value.authorization === undefined || isAuthorization(value.authorization));
}

function isAnswer(value: unknown): value is QuestionAnswer {
	return isRecord(value) && isToken(value.questionKey, 64) &&
		Array.isArray(value.selectedOptionIds) && value.selectedOptionIds.length <= 4 &&
		value.selectedOptionIds.every((id: unknown) => isToken(id, 64)) &&
		(value.otherText === undefined || isText(value.otherText, 4000)) &&
		(value.notes === undefined || isText(value.notes, 4000));
}

function isReceipt(value: unknown): value is QuestionReceipt {
	return isRecord(value) && (value.notes === undefined || isText(value.notes, 4000)) && isToken(value.clientAnswerId, 128) &&
		isVersion(value.questionVersion) && isDate(value.savedAt) &&
		typeof value.deliveryState === "string" && ["queued", "delivered", "failed"].includes(value.deliveryState) &&
		optionalDate(value.deliveredAt) && optionalString(value.lastError) &&
		(value.deliveryState !== "delivered" || isDate(value.deliveredAt)) &&
		Array.isArray(value.answers) && value.answers.length >= 1 && value.answers.length <= 4 &&
		value.answers.every(isAnswer);
}

function isQuestion(value: unknown): value is SessionQuestion {
	if (!isRecord(value) || !isToken(value.id, 128) || typeof value.projectId !== "string" ||
		!isText(value.originSessionId, 256) || !isText(value.originHarness, 120) || value.audience !== "user" ||
		(value.kind !== "question" && value.kind !== "authorization") || !isToken(value.clientKey, 64) ||
		!isPayload(value.payload) || typeof value.state !== "string" || !["open", "answered", "cancelled", "expired"].includes(value.state) ||
		!isVersion(value.version) || !isVersion(value.sequence) || typeof value.read !== "boolean" ||
		!isDate(value.createdAt) || !isDate(value.updatedAt) || !optionalDate(value.expiresAt) ||
		!optionalDate(value.closedAt) || !optionalString(value.closedBy) || !optionalString(value.closeReason) ||
		!optionalString(value.supersedesId)) return false;
	if (value.kind === "authorization" ? value.payload.authorization === undefined : value.payload.authorization !== undefined) return false;
	if (value.answer === undefined) return value.state !== "answered";
	if (value.state !== "answered" || !isReceipt(value.answer) || value.answer.questionVersion !== value.version ||
		value.answer.answers.length !== value.payload.questions.length ||
		new Set(value.answer.answers.map((answer) => answer.questionKey)).size !== value.payload.questions.length) return false;
	const fields: QuestionFields = {};
	for (const answer of value.answer.answers) {
		if (!value.payload.questions.some((prompt) => prompt.key === answer.questionKey)) return false;
		fields[answer.questionKey] = { selectedOptionIds: answer.selectedOptionIds, otherSelected: answer.otherText !== undefined, otherText: answer.otherText ?? "", notes: answer.notes ?? undefined };
	}
	return Object.keys(questionAnswers(value.payload, fields, value.answer.notes ?? undefined).errors).length === 0;
}

export function parseSessionQuestion(value: unknown, sessionId: string): SessionQuestion {
	if (!isQuestion(value) || value.originSessionId !== sessionId) throw new InvalidQuestionResponse();
	return value;
}

export function parseSessionQuestionPage(value: unknown, sessionId: string): QuestionPage {
	if (!isRecord(value) || !Array.isArray(value.items) || value.items.length > 100 ||
		!isCount(value.open) || !isCount(value.unread) || value.unread > value.open || !isCount(value.history) ||
		typeof value.hasMoreBefore !== "boolean" || !isCount(value.nextBeforeSequence) ||
		(value.hasMoreBefore && value.nextBeforeSequence === 0)) throw new InvalidQuestionResponse();
	return { items: value.items.map((item: unknown) => parseSessionQuestion(item, sessionId)),
		open: value.open, unread: value.unread, history: value.history,
		hasMoreBefore: value.hasMoreBefore, nextBeforeSequence: value.nextBeforeSequence };
}

function canonicalAnswers(answers: QuestionAnswer[]) {
	return answers.map((answer) => ({ questionKey: answer.questionKey,
		selectedOptionIds: [...answer.selectedOptionIds].sort(), otherText: answer.otherText ?? undefined, notes: answer.notes,
	})).sort((a, b) => a.questionKey.localeCompare(b.questionKey));
}

export function parseQuestionReceipt(value: unknown, expected: components["schemas"]["AttentionAnswerSubmission"]): QuestionReceipt {
	if (!isReceipt(value) || value.clientAnswerId !== expected.clientAnswerId ||
		value.questionVersion !== expected.expectedVersion || value.notes !== expected.notes ||
		JSON.stringify(canonicalAnswers(value.answers)) !== JSON.stringify(canonicalAnswers(expected.answers))) throw new InvalidQuestionResponse();
	return value;
}

export function parseQuestionSubmission(value: unknown, question: SessionQuestion): components["schemas"]["AttentionAnswerSubmission"] {
	if (!isRecord(value) || value.questionId !== question.id || value.expectedVersion !== question.version ||
		!isToken(value.clientAnswerId, 128) || (value.notes !== undefined && !isText(value.notes, 4000)) || !Array.isArray(value.answers) ||
		value.answers.length !== question.payload.questions.length) throw new InvalidQuestionResponse();
	const answers: unknown[] = value.answers;
	if (!answers.every(isAnswer) || new Set(answers.map((answer) => answer.questionKey)).size !== answers.length) throw new InvalidQuestionResponse();
	const fields: QuestionFields = {};
	for (const answer of answers) {
		if (!question.payload.questions.some((prompt) => prompt.key === answer.questionKey)) throw new InvalidQuestionResponse();
		fields[answer.questionKey] = { selectedOptionIds: answer.selectedOptionIds, otherSelected: answer.otherText !== undefined, otherText: answer.otherText ?? "", notes: answer.notes ?? undefined };
	}
	const built = questionAnswers(question.payload, fields, value.notes);
	if (Object.keys(built.errors).length) throw new InvalidQuestionResponse();
	const submission = { questionId: question.id, expectedVersion: question.version, clientAnswerId: value.clientAnswerId, answers: built.answers, ...(built.notes ? { notes: built.notes } : {}) };
	if (new TextEncoder().encode(JSON.stringify(submission)).length > 256 * 1024) throw new InvalidQuestionResponse();
	return submission;
}

export function emptyQuestionFields(payload: QuestionPayload): QuestionFields {
	return Object.fromEntries(payload.questions.map((prompt) => [prompt.key, {
		selectedOptionIds: [], otherSelected: false, otherText: "",
	}]));
}

export function questionAnswers(payload: QuestionPayload, fields: QuestionFields, notes = ""): { answers: QuestionAnswer[]; notes?: string; errors: Record<string, QuestionFieldError> } {
	const answers: QuestionAnswer[] = [];
	const errors: Record<string, QuestionFieldError> = {};
	if (notes.trim() && Array.from(notes).length > 4000) errors.$notes = "notesTooLong";
	for (const prompt of payload.questions) {
		const field = fields[prompt.key];
		if (!field || (!field.selectedOptionIds.length && !field.otherSelected)) { errors[prompt.key] = "required"; continue; }
		if (new Set(field.selectedOptionIds).size !== field.selectedOptionIds.length ||
			field.selectedOptionIds.some((id) => !prompt.options.some((option) => option.id === id))) { errors[prompt.key] = "unknown"; continue; }
		if (!prompt.multiSelect && field.selectedOptionIds.length + Number(field.otherSelected) !== 1) { errors[prompt.key] = "single"; continue; }
		if (field.otherSelected && !field.otherText.trim()) { errors[prompt.key] = "otherRequired"; continue; }
		if (field.otherSelected && Array.from(field.otherText).length > 4000) { errors[prompt.key] = "otherTooLong"; continue; }
		if (field.notes?.trim() && Array.from(field.notes).length > 4000) { errors[prompt.key] = "notesTooLong"; continue; }
		answers.push({ questionKey: prompt.key,
			selectedOptionIds: prompt.options.filter((option) => field.selectedOptionIds.includes(option.id)).map((option) => option.id),
			...(field.otherSelected ? { otherText: field.otherText } : {}),
			...(field.notes?.trim() ? { notes: field.notes } : {}),
		});
	}
	return { answers: Object.keys(errors).length ? [] : answers, ...(notes.trim() ? { notes } : {}), errors };
}
