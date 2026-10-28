import { describe, expect, it } from "vitest";
import {
	findSessionLinks,
	isAoLink,
	parseProjectDocumentLink,
	projectDocumentLink,
	relativeProjectDocumentPath,
	relativeProjectDocumentLink,
} from "./session-links";

describe("project document links", () => {
	it("round trips encoded paths without a source session", () => {
		const href = projectDocumentLink("p 1", "rules/中文 a.md");
		expect(parseProjectDocumentLink(href)).toEqual({ kind: "shared", projectId: "p 1", path: "rules/中文 a.md" });
		expect(isAoLink(href)).toBe(true);
		expect(findSessionLinks(`See ${href}.`)[0]?.text).toBe(href);
	});
	it.each([
		"ao://projects/p/shared",
		"ao://projects/p/shared/",
		"ao://projects/p/shared/../secret",
		"ao://projects/p/shared/%2e%2e/secret",
		"ao://projects/p/shared/a%2fb",
		"ao://projects/p/shared/a%5cb",
		"ao://projects/%2f/shared/x",
		"ao://projects/p/shared/%00",
		"ao://projects/p/shared/%252e%252e/secret",
		"ao://projects/p/shared/a?x=1",
		"ao://projects/p/shared/a#%ZZ",
		"ao://projects/p/shared/a#%00",
		"ao://projects/p/evidence/a",
		"ao://projects/p/shared/%ZZ",
	])("rejects %s", (href) => expect(parseProjectDocumentLink(href)).toBeUndefined());
	it("resolves relative assets inside the shared root only", () => {
		expect(relativeProjectDocumentPath("handoff/doc.md", "../architecture/a%20b.png")).toBe("architecture/a b.png");
		expect(relativeProjectDocumentPath("handoff/doc.md", "../../secret")).toBeUndefined();
		expect(relativeProjectDocumentPath("handoff/doc.md", "/etc/passwd")).toBeUndefined();
		expect(relativeProjectDocumentPath("handoff/doc.md", "https://example.com/x")).toBeUndefined();
	});
	it("round trips fragments separately from confined paths", () => {
		const href = projectDocumentLink("p", "rules/a.md", "章節一");
		expect(parseProjectDocumentLink(href)).toEqual({
			kind: "shared",
			projectId: "p",
			path: "rules/a.md",
			fragment: "章節一",
		});
		expect(relativeProjectDocumentLink("handoff/doc.md", "#section")).toEqual({
			path: "handoff/doc.md",
			fragment: "section",
		});
		expect(relativeProjectDocumentLink("handoff/doc.md", "../rules/a.md#section")).toEqual({
			path: "rules/a.md",
			fragment: "section",
		});
		for (const href of [
			"../../outside.md#x",
			"%2e%2e/x#s",
			"../rules/a.md?x=1#s",
			"#%ZZ",
			"#%00",
			"https://example.com/a#x",
		]) {
			expect(relativeProjectDocumentLink("handoff/doc.md", href)).toBeUndefined();
		}
	});
});
