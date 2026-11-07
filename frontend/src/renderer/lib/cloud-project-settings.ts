import { sameSetting, settingsObject, settingsPatch } from "./project-settings-patch";
import type { CloudCpAgentProvider, CloudCpProject, CloudCpProjectAgentConfig, CloudCpProjectSettingsRequest } from "./cloud-cp";
import { CLOUD_AGENT_PROVIDERS } from "./cloud-agents";

export type CloudProjectRoleDraft = {
	agent: CloudCpAgentProvider | "";
	agentConfig: Required<CloudCpProjectAgentConfig>;
};

export type CloudProjectSettingsDraft = {
	displayName: string;
	defaultBranch: string;
	sessionPrefix: string;
	worker: CloudProjectRoleDraft;
	orchestrator: CloudProjectRoleDraft;
	reviewer: CloudProjectRoleDraft;
	autoReview: boolean;
	coder: { workspaceNamePrefix: string; parameters: Record<string, string> };
};

export function emptyCloudAgentConfig(): Required<CloudCpProjectAgentConfig> {
	return { model: "", mode: "", effort: "", permissions: "" };
}

function cloudAgent(value: unknown): CloudCpAgentProvider | "" {
	return CLOUD_AGENT_PROVIDERS.find((agent) => agent === value) ?? "";
}

export function cloudProjectSettingsDraft(project: CloudCpProject): CloudProjectSettingsDraft {
	const config = project.config;
	const coder = settingsObject(config.coder) ? config.coder : {};
	const parameters: Record<string, string> = {};
	if (settingsObject(coder.parameters))
		for (const [key, value] of Object.entries(coder.parameters)) if (typeof value === "string") parameters[key] = value;
	const role = (name: "worker" | "orchestrator"): CloudProjectRoleDraft => ({
		agent: cloudAgent(config[name]?.agent ?? config[`${name}Agent`]),
		agentConfig: { ...emptyCloudAgentConfig(), ...config[name]?.agentConfig },
	});
	return {
		displayName: project.displayName,
		defaultBranch: project.defaultBranch,
		sessionPrefix: typeof config.sessionPrefix === "string" ? config.sessionPrefix : "",
		worker: role("worker"),
		orchestrator: role("orchestrator"),
		reviewer: {
			agent: cloudAgent(config.reviewers?.[0]?.harness),
			agentConfig: { ...emptyCloudAgentConfig(), ...config.reviewers?.[0]?.agentConfig },
		},
		// Like local projects, automatic review is off until the project enables it.
		autoReview: config.autoReview ?? false,
		coder: { workspaceNamePrefix: typeof coder.workspaceNamePrefix === "string" ? coder.workspaceNamePrefix : "", parameters },
	};
}

// Both persistence adapters use the same leaf merge rules.
export function cloudProjectSettingsPatch(
	before: CloudProjectSettingsDraft,
	after: CloudProjectSettingsDraft,
): CloudCpProjectSettingsRequest {
	const document = (draft: CloudProjectSettingsDraft) => ({
		displayName: draft.displayName.trim(),
		defaultBranch: draft.defaultBranch.trim(),
		config: {
			sessionPrefix: draft.sessionPrefix.trim(),
			worker: draft.worker.agent === "" ? null : { agent: draft.worker.agent, agentConfig: draft.worker.agentConfig },
			orchestrator:
				draft.orchestrator.agent === "" ? null : { agent: draft.orchestrator.agent, agentConfig: draft.orchestrator.agentConfig },
			reviewers: draft.reviewer.agent === "" ? [] : [{ harness: draft.reviewer.agent, agentConfig: draft.reviewer.agentConfig }],
			autoReview: draft.autoReview,
		},
	});
	const patch: CloudCpProjectSettingsRequest = settingsPatch(document(before), document(after));
	const coder: NonNullable<NonNullable<CloudCpProjectSettingsRequest["config"]>["coder"]> = {};
	if (before.coder.workspaceNamePrefix.trim() !== after.coder.workspaceNamePrefix.trim())
		coder.workspaceNamePrefix = after.coder.workspaceNamePrefix.trim();
	if (!sameSetting(before.coder.parameters, after.coder.parameters)) coder.parameters = after.coder.parameters;
	if (Object.keys(coder).length) patch.config = { ...patch.config, coder };
	return patch;
}

// This map is replaced, rather than leaf-merged, by the existing Cloud endpoint.
export const CLOUD_SETTINGS_ATOMIC_PATHS = ["config.coder.parameters"];
