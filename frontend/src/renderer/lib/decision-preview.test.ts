import { describe, expect, it } from "vitest";
import { isValidDecisionPreview } from "./decision-preview";

describe("decision preview boundary", () => {
	it.each([
		"# Target\n\n**Staging** keeps the audience small.",
		"| Target | Audience |\n| --- | --- |\n| Staging | Internal |",
		"> Rollback\n\n- one\n- two\n\n~~old~~ new",
		"[Documentation](https://example.com/guide)",
		"[Local label](./guide.md)",
		"https://example.com/guide",
		"Contact user@example.com",
		"```html\n<script>alert(1)</script>\n```",
		"`<img src=local.png>`",
	])("accepts safe Markdown without executing it: %s", (content) => {
		expect(isValidDecisionPreview({ format: "markdown", content })).toBe(true);
	});

	it.each([
		"<script>alert(1)</script>",
		"<iframe src='https://example.com'></iframe>",
		"<!-- hidden HTML -->",
		"![image](https://example.com/image.png)",
		"![image][ref]\n\n[ref]: file:///private/file",
		"[Run](javascript:alert(1))",
		"[Run](javascr&#x69;pt:alert(1))",
		"[Read](file:///private/file)",
		"[Load](data:text/html,hello)",
		"[Open](custom-widget:action)",
		"<user@example.com>",
	])("rejects unsafe Markdown: %s", (content) => {
		expect(isValidDecisionPreview({ format: "markdown", content })).toBe(false);
	});

	it("counts Unicode code points and treats text as a literal", () => {
		expect(isValidDecisionPreview({ format: "text", content: "<script>alert(1)</script>" })).toBe(true);
		expect(isValidDecisionPreview({ format: "text", content: "🚀".repeat(8192) })).toBe(true);
		expect(isValidDecisionPreview({ format: "text", content: "🚀".repeat(8193) })).toBe(false);
		for (const value of [null, {}, { format: "html", content: "hello" }, { format: "text", content: " " }, { format: "markdown", content: 3 }])
			expect(isValidDecisionPreview(value)).toBe(false);
	});
});
