import { type QueryClient, useQueries, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo } from "react";
import { clientForSessionHost } from "../lib/host-clients";
import { apiErrorMessage } from "../lib/api-client";
import { parseQuestionReceipt, type QuestionReceipt, type SessionQuestion } from "../lib/session-questions";
import { useQuestionDraftStore, type QuestionDraft } from "../stores/question-draft-store";
import { QuestionNotFound, questionSourceQueryOptions, sessionQuestionQueryKey } from "./useSessionQuestions";

const sending = new Set<string>();
const settling = new Set<string>();
const isPending = (entry: QuestionDraft | undefined) =>
	entry?.phase.state === "submitting" || entry?.phase.state === "unknown";

function responseFieldErrors(value: unknown): Record<string, string> | undefined {
	if (typeof value !== "object" || value === null || !("fieldErrors" in value) ||
		typeof value.fieldErrors !== "object" || value.fieldErrors === null) return;
	return Object.fromEntries(Object.entries(value.fieldErrors).filter((entry): entry is [string, string] => typeof entry[1] === "string"));
}

async function savedAnswer(client: QueryClient, key: string, receipt: QuestionReceipt, source?: SessionQuestion) {
	const entry = useQuestionDraftStore.getState().entries[key];
	if (!entry || (entry.phase.state !== "submitting" && entry.phase.state !== "unknown")) return;
	const expected = entry.phase.answer;
	const confirmed = parseQuestionReceipt(receipt, expected);
	const ticket = JSON.stringify([key, expected.clientAnswerId]);
	if (settling.has(ticket)) return;
	settling.add(ticket);
	try {
		const options = questionSourceQueryOptions(entry.scope);
		await client.cancelQueries({ queryKey: options.queryKey, exact: true });
		const current = useQuestionDraftStore.getState().entries[key];
		if (!current || (current.phase.state !== "submitting" && current.phase.state !== "unknown") ||
			current.phase.answer.clientAnswerId !== expected.clientAnswerId) return;
		client.setQueryData(options.queryKey, (previous) => {
			const base = source ?? (previous?.version === expected.expectedVersion ? previous : entry.question);
			const prior = previous?.answer;
			const answer = prior?.clientAnswerId === confirmed.clientAnswerId && prior.deliveryState !== "queued" && confirmed.deliveryState === "queued" ? prior : confirmed;
			return { ...base, state: "answered", answer,
				closedAt: base.closedAt ?? confirmed.savedAt, closedBy: base.closedBy ?? "user",
				closeReason: base.closeReason ?? "answer_saved", updatedAt: base.closedAt ? base.updatedAt : confirmed.savedAt,
			} satisfies SessionQuestion;
		});
		useQuestionDraftStore.getState().clearSaved(key, expected.clientAnswerId);
		await client.invalidateQueries({ queryKey: sessionQuestionQueryKey(entry.scope.sessionId, entry.scope.hostId) }, { cancelRefetch: false });
	} finally { settling.delete(ticket); }
}

// This request outlives the Dialog. Closing or switching sessions never aborts
// a submitted answer; uncertainty is recovered using its immutable client ID.
export async function sendQuestionAnswer(client: QueryClient, key: string): Promise<void> {
	const entry = useQuestionDraftStore.getState().entries[key];
	if (!entry || entry.phase.state !== "submitting") return;
	const answer = structuredClone(entry.phase.answer);
	const ticket = JSON.stringify([key, answer.clientAnswerId]);
	if (sending.has(ticket)) return;
	sending.add(ticket);
	const abort = new AbortController();
	const timeout = window.setTimeout(() => abort.abort(), 15_000);
	try {
		const { data, error, response } = await clientForSessionHost(entry.scope.hostId).POST("/api/v1/sessions/{sessionId}/questions/{questionId}/answer", {
			params: { path: { sessionId: entry.scope.sessionId, questionId: entry.scope.questionId } }, body: answer, signal: abort.signal,
		});
		if (response.status === 409 || response.status === 404) {
			useQuestionDraftStore.getState().conflict(key, answer.clientAnswerId, apiErrorMessage(error, "Question changed or is unavailable."));
			await client.invalidateQueries({ queryKey: sessionQuestionQueryKey(entry.scope.sessionId, entry.scope.hostId) }, { cancelRefetch: false });
			return;
		}
		if (response.status === 400 && error?.code === "QUESTION_INVALID") {
			useQuestionDraftStore.getState().rejected(key, answer.clientAnswerId, apiErrorMessage(error, "Check your answers."), responseFieldErrors(error.details));
			return;
		}
		if (error || !data) throw new Error(apiErrorMessage(error, "The save result could not be confirmed."));
		await savedAnswer(client, key, parseQuestionReceipt(data, answer));
	} catch (error) {
		useQuestionDraftStore.getState().unknown(key, answer.clientAnswerId, error instanceof Error ? error.message : "The save result could not be confirmed.", Date.now());
	} finally {
		window.clearTimeout(timeout);
		sending.delete(ticket);
	}
}

export function useQuestionAnswerRecovery(): void {
	const client = useQueryClient();
	const entries = useQuestionDraftStore((state) => state.entries);
	const sources = useMemo(() => {
		const grouped = new Map<string, { scope: QuestionDraft["scope"]; keys: string[]; pending: boolean }>();
		for (const [key, entry] of Object.entries(entries)) {
			const sourceKey = JSON.stringify([entry.scope.hostId ?? null, entry.scope.sessionId, entry.scope.questionId]);
			const source = grouped.get(sourceKey) ?? { scope: entry.scope, keys: [], pending: false };
			source.keys.push(key); source.pending ||= Boolean(isPending(entry)); grouped.set(sourceKey, source);
		}
		return [...grouped.values()];
	}, [entries]);
	useEffect(() => {
		const sync = (event: StorageEvent) => {
			if (event.storageArea === window.localStorage) useQuestionDraftStore.getState().syncStorage(event.key, event.newValue);
		};
		window.addEventListener("storage", sync);
		const timer = window.setInterval(() => useQuestionDraftStore.getState().prune(), 60_000);
		return () => { window.removeEventListener("storage", sync); window.clearInterval(timer); };
	}, []);
	const queries = useQueries({
		queries: sources.map((source) => ({
			...questionSourceQueryOptions(source.scope),
			staleTime: source.pending ? 0 : Infinity,
			refetchInterval: source.pending ? 2000 : 30_000,
		})),
	});
	useEffect(() => {
		for (const [index, source] of sources.entries()) {
			const query = queries[index];
			for (const key of source.keys) {
				const entry = useQuestionDraftStore.getState().entries[key];
				if (!entry || (entry.phase.state !== "submitting" && entry.phase.state !== "unknown")) continue;
				const expected = entry.phase.answer;
				if (query?.error instanceof QuestionNotFound) {
					useQuestionDraftStore.getState().conflict(key, expected.clientAnswerId, "Source is unavailable; the previous save result cannot be confirmed."); continue;
				}
				if (!query?.isSuccess || query.isFetching || !query.data) continue;
				const question = query.data;
				if (question.answer) {
					try {
						const receipt = parseQuestionReceipt(question.answer, expected);
						void savedAnswer(client, key, receipt, question).catch((error: unknown) => useQuestionDraftStore.getState().unknown(key, expected.clientAnswerId, error instanceof Error ? error.message : "Could not confirm saved answer.", Date.now()));
					} catch {
						useQuestionDraftStore.getState().conflict(key, expected.clientAnswerId, "Another answer is already saved; your previous draft is retained for reference.");
					}
					continue;
				}
				if (question.version !== expected.expectedVersion || question.state !== "open") {
					useQuestionDraftStore.getState().conflict(key, expected.clientAnswerId, "Question changed; review the latest version before answering."); continue;
				}
				if (entry.phase.state === "unknown" && useQuestionDraftStore.getState().retry(key, Date.now())) void sendQuestionAnswer(client, key);
			}
			if (query?.error instanceof QuestionNotFound) useQuestionDraftStore.getState().reconcileSource(source.scope, null);
			else if (query?.isSuccess && !query.isFetching && query.data) useQuestionDraftStore.getState().reconcileSource(source.scope, query.data);
		}
	}, [client, sources, queries]);
}

export function QuestionAnswerRuntime() {
	useQuestionAnswerRecovery();
	return null;
}
