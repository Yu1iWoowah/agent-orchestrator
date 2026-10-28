import { describe, expect, it } from "vitest";
import { appCatalogs, enMessages } from "./all-messages";
import { createLazyAppI18n } from "./instance";

describe("Traditional Chinese UI", () => {
	it("has exactly the English keys and matching value shapes", () => {
		const catalog = appCatalogs["zh-TW"];
		expect(catalog).toBeDefined();
		expect(Object.keys(catalog).sort()).toEqual(Object.keys(enMessages).sort());
		for (const [key, value] of Object.entries(enMessages)) {
			expect(Array.isArray(catalog[key]), key).toBe(Array.isArray(value));
			if (Array.isArray(value)) expect(catalog[key]).toHaveLength(value.length);
		}
	});

	it("keeps AO core terms in English singular form", () => {
		const terms = [
			"session", "worker", "orchestrator", "reviewer", "artifact", "worktree", "harness", "agent",
		];
		const prose = (value: string) =>
			value.replace(/`[^`]*`|{{.*?}}|Agent Orchestrator|Orchestrator\.inc|sub-agent/gi, "");
		for (const [key, english] of Object.entries(enMessages)) {
			const translated = appCatalogs["zh-TW"][key];
			const sources = Array.isArray(english) ? english : [english];
			const targets = Array.isArray(translated) ? translated : [translated];
			for (const [index, source] of sources.entries()) {
				const target = prose(targets[index]);
				for (const term of terms) {
					if (new RegExp(`\\b${term}(?:s|es)?\\b`, "i").test(prose(source))) {
						expect(target, `${key}: ${term}`).toMatch(new RegExp(`\\b${term}\\b`, "i"));
					}
					expect(target, `${key}: plural ${term}`).not.toMatch(new RegExp(`\\b${term}(?:s|es)\\b`, "i"));
				}
			}
		}
	});

	it("loads a real Traditional Chinese catalog before switching", async () => {
		const instance = createLazyAppI18n();
		await instance.changeLanguage("zh-TW");
		expect(instance.language).toBe("zh-TW");
		expect(instance.t("settings.general")).toBe("一般");
		expect(instance.t("settings.language.zhTW")).toBe("繁體中文");
		expect(instance.t("files.saveFile")).toBe("儲存");
		expect(instance.t("shell.projects")).toBe("專案");
		expect(instance.t("settings.sessionInterface.terminal")).toBe("終端機");
	});
});
