import { expect, it } from "vitest";
import { agentSettingsQueryKey, agentSettingsQueryRoot } from "./agent-settings";

it("shares local aliases while keeping same-session remote hosts separate", () => {
	expect(agentSettingsQueryKey("session-1", "local")).toEqual(agentSettingsQueryKey("session-1"));
	expect(agentSettingsQueryRoot("local")).toEqual(agentSettingsQueryRoot());
	expect(agentSettingsQueryKey("session-1", "host-a")).not.toEqual(agentSettingsQueryKey("session-1", "host-b"));
	expect(agentSettingsQueryKey("session-1", "host-a")).not.toEqual(agentSettingsQueryKey("session-1", "local"));
});
