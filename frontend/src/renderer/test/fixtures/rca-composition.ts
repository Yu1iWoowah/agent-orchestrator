import type { components } from "../../../api/schema";

export function compositionFixture(): components["schemas"]["RCACompositionResponse"] {
	const buckets = ["system_developer", "tool_schema", "conversation_history", "tool_result", "retained_thinking", "other_observed"].map(
		(category) => ({ category, bytes: category === "tool_result" ? 12 : null, estimatedTokens: category === "tool_result" ? 3 : null }),
	);
	return {
		projectId: "agent-orchestrator",
		sessionId: "worker-1",
		status: "partial",
		reason: "",
		requestComposition: "UNKNOWN",
		policy: {
			project: true,
			role: null,
			session: null,
			source: "project",
			sessionOverride: "inherit",
			effectiveEnabled: true,
			emergencyStop: false,
			manifestRevision: 1,
		},
		baseline: { status: "UNKNOWN", method: "", bytes: null, estimatedTokens: null },
		canonical: {
			throughId: 1,
			nextAfterId: 1,
			hasMore: false,
			events: [{ id: 1, bindingId: 2, usageSourceId: 3, inputTokens: 42, outputTokens: null, cachedInputTokens: null }],
			metadataAsOf: "",
			sourceAsOf: "",
		},
		recorded: {
			projectId: "agent-orchestrator",
			sessionId: "worker-1",
			scopeRevision: "scope-1",
			observedAt: "2026-11-10T00:00:00Z",
			status: "partial",
			reason: "",
			snapshots: [
				{
					scopeId: "scope-1",
					usageSourceId: 3,
					bindingId: 2,
					sourceGeneration: 1,
					association: "source_only",
					method: "utf8_bytes_div4_v1",
					sourceVersion: "0.167.0",
					status: "partial",
					reasons: ["hidden_thinking"],
					startOffset: 0,
					endOffset: 100,
					analyzedBytes: 100,
					records: 2,
					buckets,
					unpaired: buckets,
					turns: [{ ordinal: 1, buckets }],
				},
			],
		},
	};
}
