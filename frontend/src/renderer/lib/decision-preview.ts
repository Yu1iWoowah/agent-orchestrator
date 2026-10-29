import type { components } from "../../api/schema";
import { unified } from "unified";
import remarkParse from "remark-parse";

const markdown = unified().use(remarkParse);
const allowedNodes = new Set([
	"root", "text", "paragraph", "heading", "strong", "emphasis", "delete",
	"list", "listItem", "blockquote", "code", "inlineCode", "thematicBreak",
	"break", "table", "tableRow", "tableCell", "link", "linkReference", "definition",
]);

function safePreviewURL(value: string): boolean {
	try {
		const url = new URL(value.replace(/[\u0000-\u0020\u007f]/g, ""), "https://decision-preview.invalid/");
		return url.protocol === "https:" || url.protocol === "http:";
	} catch {
		return false;
	}
}

export function isValidDecisionPreview(value: unknown): value is components["schemas"]["AttentionPreview"] {
	if (
		typeof value !== "object" || value === null ||
		!("format" in value) || !("content" in value) ||
		(value.format !== "text" && value.format !== "markdown") ||
		typeof value.content !== "string" || !value.content.trim() ||
		Array.from(value.content).length > 8192
	) return false;
	if (value.format === "text") return true;
	try {
		const pending: unknown[] = [markdown.parse(value.content)];
		while (pending.length) {
			const node = pending.pop();
			if (typeof node !== "object" || node === null || !("type" in node) ||
				typeof node.type !== "string" || !allowedNodes.has(node.type)) return false;
			if ((node.type === "link" || node.type === "definition") &&
				(!("url" in node) || typeof node.url !== "string" || !safePreviewURL(node.url))) return false;
			if ("children" in node) {
				if (!Array.isArray(node.children)) return false;
				pending.push(...node.children);
			}
		}
		return true;
	} catch {
		return false;
	}
}
