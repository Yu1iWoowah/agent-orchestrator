import createDOMPurify from "dompurify";
import * as css from "css-tree";
import type { DocumentTarget } from "./document-targets";
import { relativeDocumentTarget, documentFileKey } from "./document-targets";
import { classifyDocument, DocumentContentError } from "./document-content";
const RESOURCE_LIMIT = 64,
	RESOURCE_BYTES = 10 * 1024 * 1024,
	TOTAL_BYTES = 20 * 1024 * 1024;
export interface DocumentResources {
	image: (reference: string, base?: DocumentTarget) => Promise<string | null>;
	stylesheet: (
		reference: string,
		base?: DocumentTarget,
		ancestors?: ReadonlySet<string>,
	) => Promise<string>;
	css: (
		source: string,
		base?: DocumentTarget,
		ancestors?: ReadonlySet<string>,
		declarations?: boolean,
	) => Promise<string>;
	dispose: () => void;
	limited: () => boolean;
	blockedLinks: () => string[];
}
/** Each reader owns its budget, in-flight resources and blobs; no app/global URL cache. */
export function createDocumentResources(
	base: DocumentTarget,
	load: (
		target: DocumentTarget,
		options: { maxBytes: number; signal: AbortSignal },
	) => Promise<ArrayBuffer>,
	transport: "blob" | "inline-raster" = "blob",
): DocumentResources {
	const abort = new AbortController();
	const blobs = new Set<string>();
	const blocked = new Set<string>();
	let limited = false;
	let stopped = false;
	let embeddedCharacters = 0;
	const readers = new Set<FileReader>();
	const images = new Map<string, Promise<string | null>>();
	const stylesheets = new Map<string, Promise<string>>();
	const cache = new Map<string, Promise<ArrayBuffer>>();
	let count = 0,
		total = 0,
		disposed = false;
	const blob = (data: BlobPart, mime: string) => {
		if (disposed) throw new DOMException("Aborted", "AbortError");
		const url = URL.createObjectURL(new Blob([data], { type: mime }));
		blobs.add(url);
		return url;
	};
	const resolve = (ref: string, from: DocumentTarget) => {
		// Resources stay in this descriptor. Cross-source canonical links are only explicit navigation.
		if (/^(?:[a-z][a-z0-9+.-]*:|\/\/)/i.test(ref)) {
			blocked.add(ref);
			return null;
		}
		return relativeDocumentTarget(from, ref);
	};
	const stop = () => {
		limited = true;
		stopped = true;
		abort.abort();
		for (const reader of readers) reader.abort();
		return new DocumentContentError("DOCUMENT_RESOURCE_LIMIT");
	};
	// One bounded read at a time: parallel image requests cannot each spend the same remaining budget.
	let pending = Promise.resolve();
	const read = (target: DocumentTarget) => {
		const key = documentFileKey(target);
		const saved = cache.get(key);
		if (saved) return saved;
		const request = pending.then(async () => {
			if (disposed) throw new DOMException("Aborted", "AbortError");
			if (stopped || ++count > RESOURCE_LIMIT || total >= TOTAL_BYTES)
				throw stop();
			const maxBytes = Math.min(RESOURCE_BYTES, TOTAL_BYTES - total);
			let data: ArrayBuffer;
			try {
				data = await load(target, { maxBytes, signal: abort.signal });
			} catch (error) {
				if (
					error &&
					typeof error === "object" &&
					"code" in error &&
					(error.code === "DOCUMENT_TOO_LARGE" ||
						error.code === "DOCUMENT_RESOURCE_LIMIT")
				)
					limited = true;
				throw error;
			}
			if (disposed) throw new DOMException("Aborted", "AbortError");
			total += data.byteLength;
			if (data.byteLength > maxBytes) {
				limited = true;
				if (total >= TOTAL_BYTES) throw stop();
				throw new DocumentContentError("DOCUMENT_TOO_LARGE");
			}
			return data;
		});
		pending = request.then(
			() => undefined,
			() => undefined,
		);
		cache.set(key, request);
		return request;
	};
	const resources: DocumentResources = {
		limited: () => limited,
		blockedLinks: () => [...blocked],
		async image(ref, from = base) {
			const target = resolve(ref, from);
			if (!target) return null;
			const key = documentFileKey(target);
			let request = images.get(key);
			if (!request) {
				request = read(target).then(async (data) => {
					const content = classifyDocument(target.path, new Uint8Array(data));
					if (content.kind !== "image") return null;
					if (transport === "blob") return blob(data, content.mime);
					// Opaque origins cannot dereference a parent-origin blob URL. Only bytes
					// classified as passive raster formats become this inline representation.
					return new Promise<string>((resolve, reject) => {
						const reader = new FileReader();
						readers.add(reader);
						const finish = () => readers.delete(reader);
						reader.onload = () => {
							finish();
							if (disposed || typeof reader.result !== "string")
								reject(new DOMException("Aborted", "AbortError"));
							else resolve(reader.result);
						};
						reader.onerror = () => {
							finish();
							reject(reader.error);
						};
						reader.onabort = () => {
							finish();
							reject(new DOMException("Aborted", "AbortError"));
						};
						reader.readAsDataURL(new Blob([data], { type: content.mime }));
					});
				});
				images.set(key, request);
			}
			const url = await request;
			if (url && transport === "inline-raster") {
				embeddedCharacters += url.length;
				if (embeddedCharacters > 28 * 1024 * 1024) {
					stop();
					return null;
				}
			}
			return url;
		},
		async stylesheet(ref, from = base, ancestors = new Set()) {
			const target = resolve(ref, from);
			if (!target) return "";
			const key = documentFileKey(target);
			if (ancestors.has(key)) return "";
			const saved = stylesheets.get(key);
			if (saved) return saved;
			const next = new Set(ancestors);
			next.add(key);
			const request = read(target).then((data) => {
				const content = classifyDocument(target.path, new Uint8Array(data));
				return "text" in content
					? resources.css(content.text, target, next)
					: "";
			});
			stylesheets.set(key, request);
			return request;
		},
		async css(
			source,
			from = base,
			ancestors = new Set(),
			declarations = false,
		) {
			let tree: css.CssNode;
			try {
				tree = css.parse(source, {
					context: declarations ? "declarationList" : "stylesheet",
					parseCustomProperty: true,
				});
			} catch {
				return "";
			}
			const imports: css.Atrule[] = [];
			const unsafe = (value: css.CssNode) => {
				let rejected = false;
				css.walk(value, (node) => {
					if (
						node.type === "Raw" ||
						(node.type === "Function" &&
							/^(?:url|src|image-set|-webkit-image-set|image|cross-fade|-webkit-cross-fade|element|-moz-element|paint)$/i.test(
								css.ident.decode(node.name),
							))
					)
						rejected = true;
				});
				return rejected;
			};
			const prune: css.EnterOrLeaveFn = function (node, item, list) {
				if (node.type === "Atrule") {
					const name = css.ident.decode(node.name).toLowerCase();
					if (name === "import") imports.push(node);
					if (
						name === "import" ||
						!["media", "supports", "layer", "container"].includes(name) ||
						(node.prelude && unsafe(node.prelude))
					) {
						list.remove(item);
						return this.skip;
					}
				} else if (
					(node.type === "Declaration" && unsafe(node.value)) ||
					(node.type === "Rule" && unsafe(node.prelude)) ||
					node.type === "Raw"
				) {
					list.remove(item);
					return this.skip;
				}
				return undefined;
			};
			css.walk(tree, prune);
			const imported: string[] = [];
			for (const rule of imports) {
				const nodes =
					rule.prelude?.type === "AtrulePrelude"
						? rule.prelude.children.toArray()
						: [];
				const ref = nodes[0];
				if (
					nodes.length === 1 &&
					(ref?.type === "String" || ref?.type === "Url")
				) {
					try {
						imported.push(
							await resources.stylesheet(ref.value, from, ancestors),
						);
					} catch {
						/* missing resource is a placeholder */
					}
				}
			}
			// @import URL nodes were removed; do not load them again as raster images.
			const remaining: css.Url[] = [];
			css.walk(tree, (node) => {
				if (node.type === "Url") remaining.push(node);
			});
			for (const node of remaining) {
				try {
					node.value = (await resources.image(node.value, from)) ?? "";
				} catch {
					node.value = "";
				}
			}
			// A style element serializes raw text; never let CSS strings terminate it.
			const result = (imported.join("\n") + css.generate(tree)).replace(
				/<\/style/gi,
				(match) => "\\3c " + match.slice(1),
			);
			if (result.length > 28 * 1024 * 1024) {
				throw stop();
			}
			return result;
		},
		dispose() {
			disposed = true;
			abort.abort();
			for (const reader of readers) reader.abort();
			readers.clear();
			for (const url of blobs) URL.revokeObjectURL(url);
			blobs.clear();
			images.clear();
			stylesheets.clear();
			cache.clear();
		},
	};
	return resources;
}
export async function staticDocumentHtml(
	source: string,
	resources: DocumentResources,
) {
	const purifier = createDOMPurify(window);
	const clean = purifier.sanitize(source, {
		WHOLE_DOCUMENT: true,
		FORBID_TAGS: [
			"script",
			"base",
			"meta",
			"form",
			"input",
			"button",
			"textarea",
			"select",
			"iframe",
			"object",
			"embed",
			"svg",
			"math",
			"video",
			"audio",
			"source",
			"area",
		],
		FORBID_ATTR: [
			"srcset",
			"action",
			"formaction",
			"target",
			"download",
			"ping",
			"poster",
			"background",
		],
		ADD_TAGS: ["link", "style"],
		ADD_ATTR: ["rel"],
	});
	const doc = new DOMParser().parseFromString(clean, "text/html");
	const links: { label: string; href: string }[] = [];
	for (const anchor of doc.querySelectorAll("a[href]")) {
		const href = anchor.getAttribute("href")!;
		if (!href.startsWith("#")) {
			links.push({ label: anchor.textContent || href, href });
			anchor.removeAttribute("href");
		}
	}
	for (const element of doc.querySelectorAll("[src]")) {
		const src = element.getAttribute("src")!;
		element.removeAttribute("src");
		if (element.tagName !== "IMG") continue;
		try {
			const safe = await resources.image(src);
			if (safe) element.setAttribute("src", safe);
			else element.setAttribute("alt", element.getAttribute("alt") || src);
		} catch {
			element.setAttribute("alt", element.getAttribute("alt") || src);
		}
	}
	for (const style of doc.querySelectorAll("style"))
		style.textContent = await resources.css(style.textContent ?? "");
	for (const link of doc.querySelectorAll("link")) {
		if (link.getAttribute("rel")?.toLowerCase() === "stylesheet") {
			try {
				const content = await resources.stylesheet(
					link.getAttribute("href") ?? "",
				);
				const style = doc.createElement("style");
				style.textContent = content;
				link.replaceWith(style);
			} catch {
				link.remove();
			}
		} else link.remove();
	}
	for (const element of doc.querySelectorAll("[style]"))
		element.setAttribute(
			"style",
			await resources.css(
				element.getAttribute("style")!,
				undefined,
				undefined,
				true,
			),
		);
	const csp = doc.createElement("meta");
	csp.httpEquiv = "Content-Security-Policy";
	csp.content =
		"default-src 'none'; script-src 'none'; connect-src 'none'; img-src data: blob:; style-src 'unsafe-inline' blob:; font-src 'none'; frame-src 'none'; form-action 'none'; base-uri 'none'";
	doc.head.prepend(csp);
	for (const href of resources.blockedLinks())
		if (/^https?:/i.test(href) && !links.some((link) => link.href === href))
			links.push({ label: href, href });
	return { html: "<!doctype html>" + doc.documentElement.outerHTML, links };
}
