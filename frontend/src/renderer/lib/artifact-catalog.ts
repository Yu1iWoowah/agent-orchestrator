import type { components } from "../../api/schema";
import { fetchDocumentCatalog, DocumentReadError, type DocumentConnection } from "./document-read";

export type ProjectCatalogResult = {
	projectId: string;
	items: components["schemas"]["DocumentCatalogItem"][];
	errors: components["schemas"]["DocumentSourceError"][];
	incomplete: boolean;
	loading: boolean;
	error?: unknown;
};
const pendingProject = (projectId: string): ProjectCatalogResult => ({
	projectId,
	items: [],
	errors: [],
	incomplete: false,
	loading: true,
});

export async function readProjectCatalog(
	connection: DocumentConnection,
	projectId: string,
	signal: AbortSignal,
	progress?: (value: ProjectCatalogResult) => void,
): Promise<ProjectCatalogResult> {
	let result = pendingProject(projectId);
	let cursor: string | undefined, revision: string | undefined;
	const seen = new Set<string>();
	try {
		do {
			if (signal.aborted) throw new DOMException("Aborted", "AbortError");
			const page = await fetchDocumentCatalog(
				connection,
				projectId,
				{ source: "all", archive: "all", limit: 200, cursor },
				signal,
			);
			if (signal.aborted) throw new DOMException("Aborted", "AbortError");
			if ((revision !== undefined && revision !== page.revision) || (page.nextCursor && seen.has(page.nextCursor)))
				throw new DocumentReadError("DOCUMENT_CATALOG_CHANGED", "Refresh the catalog");
			if (
				page.items.some(
					(item) =>
						item.target.projectId !== projectId || (item.source === "shared") !== (item.target.kind === "shared"),
				)
			)
				throw new DocumentReadError("DOCUMENT_CATALOG_INVALID", "Invalid catalog source");
			revision = page.revision;
			result = {
				projectId,
				items: [...result.items, ...page.items],
				errors: page.errors,
				incomplete: result.incomplete || page.incomplete,
				loading: Boolean(page.nextCursor),
			};
			progress?.(result);
			cursor = page.nextCursor;
			if (cursor) seen.add(cursor);
		} while (cursor);
		return result;
	} catch (error) {
		if (signal.aborted) throw error;
		result = { ...result, loading: false, incomplete: true, error };
		progress?.(result);
		return result;
	}
}

/** At most three metadata inventories in flight, including all their continuation pages. */
export async function readArtifactCatalogs(
	connection: DocumentConnection,
	projectIds: readonly string[],
	signal: AbortSignal,
	progress?: (value: ProjectCatalogResult[]) => void,
): Promise<ProjectCatalogResult[]> {
	if (connection.kind !== "local")
		throw new DocumentReadError("DOCUMENT_LOCAL_ONLY", "Local catalogs require a local connection");
	const results = projectIds.map(pendingProject);
	let next = 0;
	await Promise.all(
		Array.from({ length: Math.min(3, projectIds.length) }, async () => {
			while (next < projectIds.length) {
				if (signal.aborted) throw new DOMException("Aborted", "AbortError");
				const index = next++;
				results[index] = await readProjectCatalog(connection, projectIds[index], signal, (value) => {
					results[index] = value;
					if (!signal.aborted) progress?.([...results]);
				});
			}
		}),
	);
	return results;
}
