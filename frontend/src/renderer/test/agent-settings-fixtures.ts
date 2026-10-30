import type { components } from "../../api/schema";

type Selection = components["schemas"]["AgentSettings"];
type Operation = components["schemas"]["AgentSettingsOperation"];
type Snapshot = components["schemas"]["SessionAgentSettingsResponse"];

export function agentSettingsSelection(overrides: Partial<Selection> = {}): Selection {
	return { model: "committed-model", effort: "low", fastMode: "off", ...overrides };
}

export function agentSettingsOperation(overrides: Partial<Operation> = {}): Operation {
	return {
		id: "settings-op-1", sessionId: "sess-1", baseRevision: 4, version: 1,
		controllerGeneration: "controller-1", actor: { kind: "local_operator_unverified" },
		phase: "pending", checkpoint: "queued", applyMode: "after-turn",
		source: agentSettingsSelection(),
		target: agentSettingsSelection({ model: "pending-model", effort: "high", fastMode: "on" }),
		createdAt: "2026-10-30T09:00:00Z", updatedAt: "2026-10-30T09:00:00Z",
		...overrides,
	};
}

export function agentSettingsSnapshot(overrides: Partial<Snapshot> = {}): Snapshot {
	return {
		sessionId: "sess-1", revision: 4, settings: agentSettingsSelection(),
		capabilities: {
			state: "supported", controllerGeneration: "controller-1", generation: 1,
			models: [
				{ model: "committed-model", isDefault: true, efforts: ["low", "high"], fast: "supported" },
				{ model: "pending-model", isDefault: false, efforts: ["low", "high"], fast: "supported" },
			],
		},
		currentTurn: { state: "known", turnId: "turn-1", settings: agentSettingsSelection(), revision: 4 },
		pending: agentSettingsOperation(),
		effective: {
			model: { state: "known", value: "committed-model" },
			effort: { state: "known", value: "low" },
			fastMode: { state: "not_provided", reason: "provider_not_provided" },
		},
		...overrides,
	};
}
