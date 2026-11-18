import { describe, expect, it } from "vitest";
import { appCatalogs } from "./all-messages";

describe("Report a problem translations", () => {
	it.each(Object.entries(appCatalogs))("has the report action and feedback in %s", (_, catalog) => {
		for (const key of ["report.heading", "report.action", "report.description", "report.opening", "report.failed", "report.linkFailed", "report.github", "report.discord", "report.email"] as const) {
			expect(catalog[key]).toBeTruthy();
		}
	});
});

it.each(Object.entries(appCatalogs).filter(([locale]) => locale !== "en"))("translates report feedback in %s", (_, catalog) => {
 for (const key of ["report.heading", "report.action", "report.description", "report.opening", "report.failed", "report.linkFailed"] as const) {
  expect(catalog[key]).not.toBe(appCatalogs.en[key]);
 }
});
