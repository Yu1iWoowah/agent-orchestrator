import { describe, expect, it } from "vitest";
import {
	classifyDocument,
	DOCUMENT_TEXT_LIMIT,
	DocumentContentError,
} from "./document-content";
const bytes = (s: string) => new TextEncoder().encode(s);
describe("document byte policy", () => {
	it("uses bytes rather than trusting the filename", () => {
		expect(classifyDocument("readme.md", bytes("# safe")).kind).toBe(
			"markdown",
		);
		expect(
			classifyDocument("image.svg", bytes('<svg onload="x()"/>')).kind,
		).toBe("source");
		expect(classifyDocument("a.html", bytes("<script>x()</script>")).kind).toBe(
			"html",
		);
		expect(classifyDocument("a.md", new Uint8Array([0, 255, 3])).kind).toBe(
			"binary",
		);
		expect(classifyDocument("a.dat", bytes("%PDF-1.7\n")).kind).toBe("pdf");
	});
	it("checks text and encoded image dimensions before decoding", () => {
		expect(() =>
			classifyDocument(
				"a.txt",
				new Uint8Array(DOCUMENT_TEXT_LIMIT + 1).fill(65),
			),
		).toThrow(DocumentContentError);
		const png = new Uint8Array(24);
		png.set([137, 80, 78, 71, 13, 10, 26, 10]);
		const view = new DataView(png.buffer);
		view.setUint32(16, 10000);
		view.setUint32(20, 10000);
		expect(() => classifyDocument("tiny.png", png)).toThrow(
			"DOCUMENT_IMAGE_PIXELS",
		);
		view.setUint32(16, 10);
		view.setUint32(20, 20);
		expect(classifyDocument("tiny.png", png)).toMatchObject({
			kind: "image",
			width: 10,
			height: 20,
			mime: "image/png",
		});
	});
});
