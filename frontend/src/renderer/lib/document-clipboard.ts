import { aoBridge } from "./bridge";
import {
	resolveDocument,
	type DocumentInput,
	type DocumentConnection,
} from "./document-read";
/** Revalidate before copying, and never turn a missing browser clipboard into success. */
export async function copyDocument(
	connection: DocumentConnection,
	input: DocumentInput,
	kind: "path" | "link",
	originalLink?: string,
) {
	let text: string;
	let resolved = true;
	try {
		const document = await resolveDocument(connection, input);
		text = kind === "path" ? document.localPath : document.canonicalUri;
	} catch (error) {
		if (kind !== "link") throw error;
		resolved = false;
		const suffix =
			input.kind !== "local-path" && input.fragment != null
				? `#${encodeURIComponent(input.fragment)}`
				: "";
		const path =
			input.kind !== "local-path"
				? input.path.split("/").map(encodeURIComponent).join("/")
				: "";
		text =
			originalLink ??
			(input.kind === "local-path"
				? input.value
				: input.kind === "artifact"
					? `ao://sessions/${encodeURIComponent(input.projectId)}/${encodeURIComponent(input.sessionId)}/artifacts/${path}${suffix}`
					: `ao://projects/${encodeURIComponent(input.projectId)}/shared/${path}${suffix}`);
	}
	await writeDocumentText(text);
	return resolved;
}
export async function writeDocumentText(text: string) {
	if (!window.ao && !navigator.clipboard?.writeText)
		throw new Error("Clipboard unavailable");
	await aoBridge.clipboard.writeText(text);
}
