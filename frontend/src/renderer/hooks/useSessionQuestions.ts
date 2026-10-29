import { queryOptions, useInfiniteQuery } from "@tanstack/react-query";
import type { paths } from "../../api/schema";
import { clientForSessionHost } from "../lib/host-clients";
import { apiErrorMessage } from "../lib/api-client";
import { parseSessionQuestion, parseSessionQuestionPage } from "../lib/session-questions";
import { sessionAttentionQueryKey } from "./useSessionAttention";

export class QuestionsUnavailable extends Error {}
export class QuestionNotFound extends Error {}
export const sessionQuestionQueryKey = (sessionId: string, hostId?: string) =>
	[...sessionAttentionQueryKey(sessionId, hostId), "agent-questions"] as const;

function questionError(status: number, code: string | undefined) {
	if (status === 501 || (status === 404 && code !== "QUESTION_NOT_FOUND" && code !== "SESSION_NOT_FOUND")) throw new QuestionsUnavailable();
	if (status === 404) throw new QuestionNotFound();
}

export function questionSourceQueryOptions({ sessionId, hostId, questionId }: { sessionId: string; hostId?: string; questionId: string }) {
	return queryOptions({
		queryKey: [...sessionQuestionQueryKey(sessionId, hostId), "question", questionId],
		queryFn: async ({ signal }) => {
			const { data, error, response } = await clientForSessionHost(hostId).GET("/api/v1/sessions/{sessionId}/questions/{questionId}", { params: { path: { sessionId, questionId } }, signal });
			questionError(response.status, error?.code);
			if (error || !data) throw new Error(apiErrorMessage(error, "Could not load this question."));
			const question = parseSessionQuestion(data, sessionId);
			if (question.id !== questionId) throw new QuestionNotFound();
			return question;
		},
		staleTime: Infinity,
		networkMode: "always",
		retry: false,
	});
}

export function useSessionQuestions(sessionId: string, hostId: string | undefined, state: "open" | "history", enabled = true) {
	return useInfiniteQuery({
		queryKey: [...sessionQuestionQueryKey(sessionId, hostId), "list", state],
		enabled,
		initialPageParam: 0,
		queryFn: async ({ pageParam, signal }) => {
			const query = { state, beforeSequence: pageParam || undefined, limit: 20 } satisfies paths["/api/v1/sessions/{sessionId}/questions"]["get"]["parameters"]["query"];
			const { data, error, response } = await clientForSessionHost(hostId).GET("/api/v1/sessions/{sessionId}/questions", { params: { path: { sessionId }, query }, signal });
			questionError(response.status, error?.code);
			if (error || !data) throw new Error(apiErrorMessage(error, "Could not load questions."));
			return parseSessionQuestionPage(data, sessionId);
		},
		getNextPageParam: (page) => page.hasMoreBefore ? page.nextBeforeSequence : undefined,
		networkMode: "always",
		retry: false,
	});
}
