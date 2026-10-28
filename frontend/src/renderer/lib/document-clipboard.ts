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
) {
	const document = await resolveDocument(connection, input);
	if (!window.ao && !navigator.clipboard?.writeText)
		throw new Error("Clipboard unavailable");
	await aoBridge.clipboard.writeText(
		kind === "path" ? document.localPath : document.canonicalUri,
	);
}
