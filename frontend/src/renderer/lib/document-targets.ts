import type { components } from "../../api/schema";
import type { DocumentInput, DocumentTarget } from "./document-read";
export type ResolvedDocument = components["schemas"]["ResolvedDocument"];
export type { DocumentInput, DocumentTarget };

function decodedSegment(value: string): string | null {
	try {
		const result = decodeURIComponent(value);
		return !result ||
			result === "." ||
			result === ".." ||
			/[/\\\x00-\x1f\x7f]/.test(result)
			? null
			: result;
	} catch {
		return null;
	}
}
function fragment(value: string | undefined): string | undefined | null {
	if (value === undefined) return undefined;
	try {
		const result = decodeURIComponent(value);
		return /[\x00-\x1f\x7f]/.test(result) ? null : result;
	} catch {
		return null;
	}
}
/** Canonical URI transport is decoded exactly once; target.path is always a literal daemon path. */
export function documentInputFromLink(value: string): DocumentInput | null {
	if (/^(?:file:|\/|~\/|[a-z]:[\\/])/i.test(value))
		return { kind: "local-path", value };
	const match = /^ao:\/\/(sessions|projects)\/([^?#]*)(?:#(.*))?$/.exec(value);
	if (!match) return null;
	const parts = match[2].split("/");
	const projectId = decodedSegment(parts[0] ?? "");
	const anchor = fragment(match[3]);
	if (!projectId || anchor === null) return null;
	const tailStart = match[1] === "sessions" ? 3 : 2;
	const tail = parts.slice(tailStart).map(decodedSegment);
	if (!tail.length || tail.some((part) => part === null)) return null;
	const path = tail.join("/");
	const position = anchor === undefined ? {} : { fragment: anchor };
	if (match[1] === "projects")
		return parts[1] === "shared"
			? { kind: "shared", projectId, path, ...position }
			: null;
	const sessionId = decodedSegment(parts[1] ?? "");
	return parts[2] === "artifacts" && sessionId
		? { kind: "artifact", projectId, sessionId, path, ...position }
		: null;
}

/** Relative references normalize raw dot segments within one verified root, never the base target's escapes. */
export function relativeDocumentTarget(
	base: DocumentTarget,
	reference: string,
): DocumentTarget | null {
	const explicit = documentInputFromLink(reference);
	if (explicit?.kind === "artifact" || explicit?.kind === "shared")
		return explicit;
	if (
		/^(?:[a-z][a-z0-9+.-]*:|\/\/)/i.test(reference) ||
		reference.includes("?")
	)
		return null;
	const hash = reference.indexOf("#");
	const relative = hash < 0 ? reference : reference.slice(0, hash);
	const anchor = fragment(hash < 0 ? undefined : reference.slice(hash + 1));
	if (anchor === null) return null;
	let path = base.path;
	if (relative) {
		const parts = relative.startsWith("/")
			? []
			: base.path.split("/").slice(0, -1);
		const source = relative.startsWith("/") ? relative.slice(1) : relative;
		for (const part of source.split("/")) {
			if (part === ".") continue;
			if (part === "..") {
				if (!parts.length) return null;
				parts.pop();
				continue;
			}
			let decoded: string | null;
			try {
				decodeURIComponent(part);
				decoded = decodedSegment(part);
			} catch {
				decoded = /[\\\x00-\x1f\x7f]/.test(part) ? null : part;
			}
			if (decoded === null) return null;
			parts.push(decoded);
		}
		if (!parts.length) return null;
		path = parts.join("/");
	}
	const position = anchor === undefined ? {} : { fragment: anchor };
	return base.kind === "artifact"
		? {
				kind: "artifact",
				projectId: base.projectId,
				sessionId: base.sessionId,
				path,
				...position,
			}
		: { kind: "shared", projectId: base.projectId, path, ...position };
}
export function documentFileKey(target: DocumentTarget): string {
	return JSON.stringify(
		target.kind === "artifact"
			? [target.kind, target.projectId, target.sessionId, target.path]
			: [target.kind, target.projectId, target.path],
	);
}
