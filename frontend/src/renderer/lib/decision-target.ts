export type DecisionTarget =
	| { kind: "agent_question"; questionId: string; title: string }
	| { kind: "provider_approval" | "provider_input"; activityId: string; requestId: string; conversationId: string; branchId: string; sourceBranchId: string; title: string }
	| { kind: "worker_report"; reportId: string; originSessionId: string; originHarness: string; title: string; explanation: string };

export function decisionTargetKey(target: DecisionTarget): string {
	switch (target.kind) {
		case "agent_question": return JSON.stringify([target.kind, target.questionId]);
		case "provider_approval":
		case "provider_input": return JSON.stringify([target.kind, target.conversationId, target.branchId, target.sourceBranchId, target.activityId, target.requestId]);
		case "worker_report": return JSON.stringify([target.kind, target.originSessionId, target.reportId]);
	}
}
