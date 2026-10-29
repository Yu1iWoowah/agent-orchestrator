import { describe, expect, it } from "vitest";
import { accountModelQuery, localAccountModelScope } from "./local-account-model-scope";
import { agentModelsQueryKey } from "../hooks/useAgentModelsQuery";
describe("local account model scopes", () => {
	it("keeps project roles and account references separate without sending a cache key as a project ID", () => {
		const worker = localAccountModelScope("p", "worker", "A");
		const reviewer = localAccountModelScope("p", "reviewer", "C");
		expect(agentModelsQueryKey("codex", worker)).not.toEqual(agentModelsQueryKey("codex", reviewer));
		expect(agentModelsQueryKey("codex", "p")).not.toEqual(agentModelsQueryKey("codex", ""));
		expect(accountModelQuery(worker)).toEqual({ projectId: "p", role: "worker", agentAccountId: "A" });
		expect(accountModelQuery(reviewer)).toEqual({ projectId: "p", role: "reviewer", agentAccountId: "C" });
	});
	it("preserves standalone and Cloud credential queries", () => {
		expect(accountModelQuery(localAccountModelScope("", "worker", "A"))).toEqual({ projectId: undefined, role: "worker", agentAccountId: "A" });
		expect(accountModelQuery("@cred:anthropic_api_key")).toEqual({ projectId: "@cred:anthropic_api_key" });
	});
});
