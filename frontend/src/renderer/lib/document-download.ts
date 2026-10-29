import { apiClient, apiErrorMessage } from "./api-client";
import {
	readDocumentContent,
	resolveDocument,
	type DocumentConnection,
	type DocumentTarget,
} from "./document-read";
/** Explicit downloads retain existing source limits and never navigate the app to active HTML. */
export async function downloadDocument(
	connection: DocumentConnection,
	input: DocumentTarget,
) {
	const meta = await resolveDocument(connection, input);
	let data: Blob;
	if (meta.target.kind === "artifact") {
		const bytes = await readDocumentContent(
			connection,
			meta.target,
			50 * 1024 * 1024,
			meta.fingerprint,
		);
		data = new Blob([bytes], { type: "application/octet-stream" });
	} else {
		const target = meta.target;
		const response = await apiClient.GET(
			"/api/v1/projects/{id}/documents/files/*",
			{
				params: { path: { id: target.projectId } },
				pathSerializer: () =>
					`/api/v1/projects/${encodeURIComponent(target.projectId)}/documents/files/${target.path.split("/").map(encodeURIComponent).join("/")}`,
				parseAs: "blob",
			},
		);
		if (response.error) throw new Error(apiErrorMessage(response.error));
		if (!response.data) throw new Error("Download response missing");
		data = new Blob([response.data], { type: "application/octet-stream" });
	}
	const url = URL.createObjectURL(data);
	const anchor = window.document.createElement("a");
	anchor.href = url;
	anchor.download = meta.target.path.split("/").at(-1) ?? "document";
	window.document.body.append(anchor);
	anchor.click();
	anchor.remove();
	setTimeout(() => URL.revokeObjectURL(url), 1000);
}
