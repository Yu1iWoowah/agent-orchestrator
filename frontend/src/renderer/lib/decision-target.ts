import type { ProjectAttentionItem } from "../hooks/useProjectAttention";
import { LOCAL_HOST } from "./hosts";
import { newestActiveOrchestrator, type WorkspaceSummary } from "../types/workspace";

export type QuestionRef = { hostId: string; projectId: string; originSessionId: string; questionId: string };
export type QuestionHost = { kind: "unknown" } | { kind: "none" } | { kind: "orchestrator"; sessionId: string };

export function parseQuestionRef(value: unknown): QuestionRef | undefined {
	if (!value || typeof value !== "object" ||
		!("hostId" in value) || typeof value.hostId !== "string" || !value.hostId ||
		!("projectId" in value) || typeof value.projectId !== "string" || !value.projectId ||
		!("originSessionId" in value) || typeof value.originSessionId !== "string" || !value.originSessionId ||
		!("questionId" in value) || typeof value.questionId !== "string" || !value.questionId) return;
	return { hostId: value.hostId, projectId: value.projectId, originSessionId: value.originSessionId, questionId: value.questionId };
}

export function questionHost({ workspace, ready, projectId, hostId = LOCAL_HOST }: {
	workspace?: WorkspaceSummary; ready: boolean; projectId: string; hostId?: string;
}): QuestionHost {
	if (!ready || !workspace || workspace.id !== projectId || (workspace.hostId ?? LOCAL_HOST) !== hostId) return { kind: "unknown" };
	if (workspace.sessions.some((session) => typeof session.id !== "string" || !session.id || (session.kind !== "worker" && session.kind !== "orchestrator") ||
		typeof session.isTerminated !== "boolean" || typeof session.createdAt !== "string" || !Number.isFinite(Date.parse(session.createdAt)) ||
		typeof session.updatedAt !== "string" || !Number.isFinite(Date.parse(session.updatedAt))))
		return { kind: "unknown" };
	const orchestrator = newestActiveOrchestrator(workspace.sessions);
	return orchestrator ? { kind: "orchestrator", sessionId: orchestrator.id } : { kind: "none" };
}

export type DecisionTarget =
	| { kind: "agent_question"; questionId: string; title: string }
	| { kind: "provider_approval" | "provider_input"; activityId: string; requestId: string; conversationId: string; branchId: string; sourceBranchId: string; title: string }
	| { kind: "worker_report"; reportId: string; originSessionId: string; originHarness: string; title: string; explanation: string };

export type DecisionSelection = { target: DecisionTarget; sessionId: string; projectId?: string };

export function projectDecisionTarget(item: ProjectAttentionItem): DecisionTarget | undefined {
	switch (item.kind) {
		case "agent_question": return item.questionId ? { kind: item.kind, questionId: item.questionId, title: item.title } : undefined;
		case "worker_report": return item.reportId ? { kind: item.kind, reportId: item.reportId, originSessionId: item.sessionId, originHarness: item.originHarness ?? "unknown", title: item.title, explanation: item.title } : undefined;
		case "provider_approval":
		case "provider_input": return item.activityId && item.requestId && item.conversationId && item.branchId && item.sourceBranchId
			? { kind: item.kind, activityId: item.activityId, requestId: item.requestId, conversationId: item.conversationId, branchId: item.branchId, sourceBranchId: item.sourceBranchId, title: item.title } : undefined;
	}
}

export function decisionTargetKey(target: DecisionTarget): string {
	switch (target.kind) {
		case "agent_question": return JSON.stringify([target.kind, target.questionId]);
		case "provider_approval":
		case "provider_input": return JSON.stringify([target.kind, target.conversationId, target.branchId, target.sourceBranchId, target.activityId, target.requestId]);
		case "worker_report": return JSON.stringify([target.kind, target.originSessionId, target.reportId]);
	}
}
