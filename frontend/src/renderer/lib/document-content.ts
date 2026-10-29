export const DOCUMENT_TEXT_LIMIT = 2 * 1024 * 1024;
export const DOCUMENT_MEDIA_LIMIT = 20 * 1024 * 1024;
const MAX_PIXELS = 40_000_000;
export class DocumentContentError extends Error {
	constructor(readonly code: string) {
		super(code);
	}
}
export type DocumentContent =
	| { kind: "markdown" | "text" | "source" | "html"; text: string }
	| { kind: "image"; mime: string; width: number; height: number }
	| { kind: "pdf" }
	| { kind: "binary" };
const ascii = (b: Uint8Array, start: number, length: number) =>
	String.fromCharCode(...b.subarray(start, start + length));
/** Read encoded headers before handing an image to the browser decoder. */
function image(
	b: Uint8Array,
): { mime: string; width: number; height: number } | null {
	const v = new DataView(b.buffer, b.byteOffset, b.byteLength);
	if (b.length >= 24 && b[0] === 137 && ascii(b, 1, 7) === "PNG\r\n\x1a\n")
		return {
			mime: "image/png",
			width: v.getUint32(16),
			height: v.getUint32(20),
		};
	if (b.length >= 10 && /^GIF8[79]a$/.test(ascii(b, 0, 6)))
		return {
			mime: "image/gif",
			width: v.getUint16(6, true),
			height: v.getUint16(8, true),
		};
	if (b.length >= 4 && b[0] === 255 && b[1] === 216) {
		let p = 2;
		while (p + 4 <= b.length) {
			if (b[p] !== 255) break;
			const marker = b[p + 1];
			p += 2;
			if (marker === 216 || marker === 1 || (marker >= 208 && marker <= 215))
				continue;
			const size = v.getUint16(p);
			if (size < 2 || p + size > b.length) break;
			if (
				[
					192, 193, 194, 195, 197, 198, 199, 201, 202, 203, 205, 206, 207,
				].includes(marker) &&
				size >= 7
			)
				return {
					mime: "image/jpeg",
					width: v.getUint16(p + 5),
					height: v.getUint16(p + 3),
				};
			p += size;
		}
		throw new DocumentContentError("DOCUMENT_IMAGE_INVALID");
	}
	if (
		b.length >= 30 &&
		ascii(b, 0, 4) === "RIFF" &&
		ascii(b, 8, 4) === "WEBP"
	) {
		const type = ascii(b, 12, 4);
		if (type === "VP8X")
			return {
				mime: "image/webp",
				width: 1 + b[24] + (b[25] << 8) + (b[26] << 16),
				height: 1 + b[27] + (b[28] << 8) + (b[29] << 16),
			};
		if (type === "VP8 " && b[23] === 157 && b[24] === 1 && b[25] === 42)
			return {
				mime: "image/webp",
				width: v.getUint16(26, true) & 16383,
				height: v.getUint16(28, true) & 16383,
			};
		if (type === "VP8L" && b[20] === 47)
			return {
				mime: "image/webp",
				width: 1 + ((b[21] | (b[22] << 8)) & 16383),
				height: 1 + (((b[22] >> 6) | (b[23] << 2) | (b[24] << 10)) & 16383),
			};
		throw new DocumentContentError("DOCUMENT_IMAGE_INVALID");
	}
	if (
		b.length >= 16 &&
		ascii(b, 4, 4) === "ftyp" &&
		/avif|avis/.test(ascii(b, 8, Math.min(48, b.length - 8)))
	) {
		// AVIF stores spatial extents in a FullBox ispe inside meta/iprp/ipco.
		const dimensions: { width: number; height: number }[] = [];
		function boxes(start: number, end: number, depth: number) {
			if (depth > 8) return;
			for (let p = start; p + 8 <= end; ) {
				const size = v.getUint32(p);
				const type = ascii(b, p + 4, 4);
				if (size < 8 || p + size > end) break;
				if (type === "ispe" && size >= 20)
					dimensions.push({
						width: v.getUint32(p + 12),
						height: v.getUint32(p + 16),
					});
				if (["meta", "iprp", "ipco"].includes(type))
					boxes(p + 8 + (type === "meta" ? 4 : 0), p + size, depth + 1);
				p += size;
			}
		}
		boxes(0, b.length, 0);
		if (!dimensions.length)
			throw new DocumentContentError("DOCUMENT_IMAGE_INVALID");
		return {
			mime: "image/avif",
			width: Math.max(...dimensions.map((d) => d.width)),
			height: Math.max(...dimensions.map((d) => d.height)),
		};
	}
	return null;
}
export function classifyDocument(path: string, b: Uint8Array): DocumentContent {
	if (b.length > DOCUMENT_MEDIA_LIMIT)
		throw new DocumentContentError("DOCUMENT_TOO_LARGE");
	const raster = image(b);
	if (raster) {
		if (
			!raster.width ||
			!raster.height ||
			raster.width * raster.height > MAX_PIXELS
		)
			throw new DocumentContentError("DOCUMENT_IMAGE_PIXELS");
		return { kind: "image", ...raster };
	}
	if (ascii(b, 0, 5) === "%PDF-") return { kind: "pdf" };
	let text: string;
	try {
		text = new TextDecoder("utf-8", { fatal: true }).decode(b);
	} catch {
		return { kind: "binary" };
	}
	if (/[\x00-\x08\x0b\x0c\x0e-\x1f]/.test(text)) return { kind: "binary" };
	if (b.length > DOCUMENT_TEXT_LIMIT)
		throw new DocumentContentError("DOCUMENT_TOO_LARGE");
	const extension = path.split(".").at(-1)?.toLowerCase();
	return {
		kind:
			extension === "md" || extension === "markdown"
				? "markdown"
				: extension === "html" || extension === "htm"
					? "html"
					: extension === "svg"
						? "source"
						: "text",
		text,
	};
}
