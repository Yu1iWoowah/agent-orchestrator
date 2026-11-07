import { describe, expect, it } from "vitest";
import type { components } from "../../api/schema";
import { conflictingSettings, rebaseSettings, settingsExpected, settingsPatch } from "./project-settings-patch";

type Values = { config: components["schemas"]["ProjectConfig"] };

describe("project settings patches", () => {
	it("compares only the edited leaf against its stored value, not a displayed fallback", () => {
		const before: Values = { config: { worker: { agentConfig: { model: "inherited", effort: "high" } } } };
		const after: Values = { config: { worker: { agentConfig: { model: "inherited", effort: "low" } } } };
		const patch = settingsPatch(before, after);
		expect(patch).toEqual({ config: { worker: { agentConfig: { effort: "low" } } } });
		expect(settingsExpected(patch, { config: { worker: { agentConfig: {} } } })).toEqual({
			config: { worker: { agentConfig: { effort: null } } },
		});
	});
	it("removes individual leaves and records absent leaves as null", () => {
		const before: Values = { config: { worker: { agentConfig: { model: "old" } }, env: { OLD: "value" } } };
		const after: Values = { config: { worker: { agentConfig: {} }, env: { NEW: "new" } } };
		const patch = settingsPatch(before, after);
		expect(patch).toEqual({ config: { worker: { agentConfig: { model: null } }, env: { OLD: null, NEW: "new" } } });
		expect(settingsExpected(patch, before)).toEqual({
			config: { worker: { agentConfig: { model: "old" } }, env: { OLD: "value", NEW: null } },
		});
	});
	it("replaces arrays as a whole and ignores object member ordering", () => {
		const before: Values = { config: { reviewers: [{ harness: "codex", agentConfig: { model: "keep" } }], env: { A: "a", B: "b" } } };
		const after: Values = { config: { reviewers: [], env: { B: "b", A: "a" } } };
		expect(settingsPatch(before, after)).toEqual({ config: { reviewers: [] } });
		expect(settingsExpected(settingsPatch(before, after), before)).toEqual({ config: { reviewers: before.config.reviewers } });
	});
	it("detects same-leaf changes without treating unrelated changes as conflicts", () => {
		const patch = { config: { worker: { agentConfig: { model: "mine" } } } };
		const expected = { config: { worker: { agentConfig: { model: "old" } } } };
		expect(
			conflictingSettings(patch, expected, { config: { worker: { agentConfig: { model: "old", effort: "new" } }, reviewers: [] } }),
		).toEqual([]);
		expect(conflictingSettings(patch, expected, { config: { worker: { agentConfig: { model: "external" } } } })).toEqual([
			"config.worker.agentConfig.model",
		]);
	});
	it("rebases only non-conflicting user edits onto the latest values", () => {
		const before = { name: "Old", model: "old", effort: "high", prefix: "a", accounts: { codex: "old", claude: "keep" } };
		const draft = { ...before, model: "mine", prefix: "mine", accounts: { ...before.accounts, codex: "mine" } };
		const latest = { name: "External", model: "external", effort: "low", prefix: "a", accounts: { codex: "external", claude: "new" } };
		expect(rebaseSettings(before, draft, latest, ["model", "accounts.codex"])).toEqual({
			name: "External",
			model: "external",
			effort: "low",
			prefix: "mine",
			accounts: { codex: "external", claude: "new" },
		});
	});
 it("keeps legal environment names distinct from object prototype members", () => {
  const before:Values={config:{env:{}}};
  const after:Values={config:{env:Object.fromEntries([["constructor","one"],["__proto__","two"]])}};
  const patch=settingsPatch(before,after);
  expect(JSON.stringify(patch)).toBe(JSON.stringify(after));
  expect(JSON.stringify(settingsExpected(patch,before))).toBe('{"config":{"env":{"constructor":null,"__proto__":null}}}');
  expect(rebaseSettings(before,after,before,[])).toEqual(after);
  expect(conflictingSettings(patch,settingsExpected(patch,before),before)).toEqual([]);
 });

});
