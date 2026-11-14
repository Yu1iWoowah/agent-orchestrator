import { describe, expect, it } from "vitest";
import type { CloudCpProject } from "./cloud-cp";
import { cloudProjectSettingsDraft, cloudProjectSettingsPatch } from "./cloud-project-settings";

describe("Cloud settings leaf patches", () => {
	it("sends only changed worker effort, without resending its model or agent", () => {
		const project: CloudCpProject = {
			id: "p",
			orgId: "org",
			displayName: "Project",
			defaultBranch: "main",
			repositoryUrl: "https://github.com/owner/repo",
			createdAt: "now",
			updatedAt: "now",
			config: { worker: { agent: "codex", agentConfig: { model: "keep-model", effort: "high" } } },
		};
		const before = cloudProjectSettingsDraft(project);
		const after = structuredClone(before);
		after.worker.agentConfig.effort = "low";
		expect(cloudProjectSettingsPatch(before, after)).toEqual({ config: { worker: { agentConfig: { effort: "low" } } } });
	});
});
