import createDOMPurify from "dompurify";
import * as css from "css-tree";
import type { DocumentTarget } from "./document-targets";
import { relativeDocumentTarget, documentFileKey } from "./document-targets";
import { classifyDocument, DocumentContentError, DOCUMENT_TEXT_LIMIT } from "./document-content";
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
}
/** Each reader owns its budget, in-flight resources and blobs; no app/global URL cache. */
export function createDocumentResources(
	base: DocumentTarget,
	load: (target: DocumentTarget) => Promise<ArrayBuffer>,
): DocumentResources {
	const blobs = new Set<string>();
	const images = new Map<string,Promise<string|null>>();
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
		if (/^(?:[a-z][a-z0-9+.-]*:|\/\/)/i.test(ref)) return null;
		return relativeDocumentTarget(from, ref);
	};
	const read = (target: DocumentTarget) => {
		const key = documentFileKey(target);
		const saved = cache.get(key);
		if (saved) return saved;
		if (++count > RESOURCE_LIMIT)
			return Promise.reject(
				new DocumentContentError("DOCUMENT_RESOURCE_LIMIT"),
			);
		const request = load(target).then((data) => {
			if (disposed) throw new DOMException("Aborted", "AbortError");
			total += data.byteLength;
			if (data.byteLength > RESOURCE_BYTES || total > TOTAL_BYTES)
				throw new DocumentContentError("DOCUMENT_RESOURCE_LIMIT");
			return data;
		});
		cache.set(key, request);
		return request;
	};
	const resources: DocumentResources = {
		async image(ref,from=base){
			const target=resolve(ref,from);if(!target)return null;
			const key=documentFileKey(target);const saved=images.get(key);if(saved)return saved;
			const request=read(target).then(data=>{const content=classifyDocument(target.path,new Uint8Array(data));return content.kind==="image"?blob(data,content.mime):null;});
			images.set(key,request);return request;
		},
		async stylesheet(ref, from = base, ancestors = new Set()) {
			const target = resolve(ref, from);
			if (!target) return "";
			const key = documentFileKey(target);
			if (ancestors.has(key)) return "";
			const next = new Set(ancestors);
			next.add(key);
			const data = await read(target);
			const content = classifyDocument(target.path, new Uint8Array(data));
			if (!("text" in content)) return "";
			return resources.css(content.text, target, next);
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
			let invalid = false;
			css.walk(tree, (node) => {
				if (node.type === "Raw") invalid = true;
				if (
					node.type === "Function" &&
					!/^(?:rgb|rgba|hsl|hsla|hwb|lab|lch|oklab|oklch|color|color-mix|calc|min|max|clamp|linear-gradient|radial-gradient|repeating-linear-gradient|repeating-radial-gradient|translate[xyz]?|scale[xyz]?|rotate[xyz]?|matrix|cubic-bezier|steps)$/i.test(
						node.name,
					)
				)
					invalid = true;
				if (node.type === "Atrule") {
					if (node.name.toLowerCase() === "import") imports.push(node);
					else if (
						!["media", "supports", "layer"].includes(node.name.toLowerCase())
					)
						invalid = true;
				}
			});
			if (invalid) return "";
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
				rule.name = "media";
				rule.prelude = {
					type: "AtrulePrelude",
					children: new css.List<css.CssNode>().fromArray([
						{ type: "Identifier", name: "not" },
						{ type: "Identifier", name: "all" },
					]),
				};
				rule.block = css.parse("{}", { context: "block" }) as css.Block;
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
			const result=imported.join("\n")+css.generate(tree);
			if(result.length>DOCUMENT_TEXT_LIMIT)throw new DocumentContentError("DOCUMENT_RESOURCE_LIMIT");
			return result;
		},
		dispose() {
			disposed = true;
			for (const url of blobs) URL.revokeObjectURL(url);
			blobs.clear();
			images.clear();
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
		"default-src 'none'; script-src 'none'; connect-src 'none'; img-src blob:; style-src 'unsafe-inline' blob:; font-src 'none'; frame-src 'none'; form-action 'none'; base-uri 'none'";
	doc.head.prepend(csp);
	return { html: "<!doctype html>" + doc.documentElement.outerHTML, links };
}
