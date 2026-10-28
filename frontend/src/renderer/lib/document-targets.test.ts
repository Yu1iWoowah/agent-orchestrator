import { describe, expect, it } from "vitest";
import {
	documentInputFromLink,
	relativeDocumentTarget,
	documentFileKey,
} from "./document-targets";

const base = {
	kind: "artifact",
	projectId: "p",
	sessionId: "p-1",
	path: "folder/%2e%2e%2fx%",
} as const;
describe("document targets", () => {
	it("decodes canonical transport once without decoding the backend literal path", () => {
		expect(
			documentInputFromLink(
				"ao://sessions/p/p-1/artifacts/folder/%252e%252e%252fx%25#intro",
			),
		).toEqual({ ...base, fragment: "intro" });
		expect(relativeDocumentTarget(base, "#next")).toEqual({
			...base,
			fragment: "next",
		});
		expect(relativeDocumentTarget(base, "child.md")).toEqual({
			...base,
			path: "folder/child.md",
		});
	});
	it("preserves absolute/file input for daemon resolution", () => {
		for (const value of [
			"/owned/%2e%2e%2fx%",
			"file:///owned/%252e%252e%252fx%25",
		])
			expect(documentInputFromLink(value)).toEqual({
				kind: "local-path",
				value,
			});
	});
	it("confines relative references and rejects ambiguous transport", () => {
		expect(relativeDocumentTarget(base, "../guide.md")).toEqual({
			...base,
			path: "guide.md",
		});
		for (const value of [
			"../../escape.md",
			"x//y.md",
			"%2e%2e/x.md",
			"x%2fy.md",
			"https://evil.example/img.png",
			"//evil.example/x",
		])
			expect(relativeDocumentTarget(base, value)).toBeNull();
		for (const value of [
			"ao://sessions/p/p-1/artifacts/%2e%2e%2fx%",
			"ao://sessions/p/p-1/artifacts/x//y.md",
		])
			expect(documentInputFromLink(value)).toBeNull();
	});
	it("keeps file identity separate from explicit fragment navigation", () => {
		expect(documentFileKey(base)).toBe(
			documentFileKey({ ...base, fragment: "other" }),
		);
		expect(documentFileKey(base)).not.toBe(
			documentFileKey({ kind: "shared", projectId: "p", path: base.path }),
		);
	});
});

it("keeps a malformed relative escape entirely literal instead of partially decoding it", () => {
	const base = { kind: "shared", projectId: "p", path: "dir/base.md" } as const;
	expect(relativeDocumentTarget(base, "%2e%2e%2fx%")).toEqual({
		...base,
		path: "dir/%2e%2e%2fx%",
	});
	expect(relativeDocumentTarget(base, "50%.md")).toEqual({
		...base,
		path: "dir/50%.md",
	});
	expect(relativeDocumentTarget(base, "%2e%2e%2fx")).toBeNull();
});
