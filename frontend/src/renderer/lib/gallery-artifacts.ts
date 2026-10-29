import type { components } from "../../api/schema";
import type { DocumentConnection } from "./document-read";
import { artifactKey, groupArtifactsByProject, type ArtifactGroup, type ArtifactRow } from "./artifacts";
import { STANDALONE_WORKSPACE_ID, type WorkspaceSummary } from "../types/workspace";

export type CatalogItem = components["schemas"]["DocumentCatalogItem"];
export type CatalogArtifactRow = CatalogItem & { connection: DocumentConnection; sessionTitle?: string };
export type GalleryRow = CatalogArtifactRow | (ArtifactRow & { connection: DocumentConnection });
export type GalleryGroup = Omit<ArtifactGroup, "rows"> & { rows: GalleryRow[] };
export type GallerySource = "all" | "worker" | "orchestrator" | "shared";
export type GalleryArchive = "all" | "current" | "archive";
export type GalleryFilters = { query: string; projectId: string; source: GallerySource; archive: GalleryArchive };

export function isCatalogArtifact(row: GalleryRow): row is CatalogArtifactRow {
	return "target" in row;
}
export function galleryArtifactKey(row: GalleryRow): string {
	const projectId = isCatalogArtifact(row) ? row.target.projectId : (row.projectId ?? STANDALONE_WORKSPACE_ID);
	const owner = isCatalogArtifact(row) ? (row.target.kind === "artifact" ? row.target.sessionId : null) : row.sessionId;
	return JSON.stringify([
		"artifact-v2",
		row.connection.kind,
		row.connection.identity,
		projectId,
		isCatalogArtifact(row) ? row.source : "worker",
		owner,
		row.path,
	]);
}
export function galleryLegacyKey(row: GalleryRow): string | null {
	if (isCatalogArtifact(row))
		return row.target.kind === "artifact" ? artifactKey({ sessionId: row.target.sessionId, path: row.path }) : null;
	return artifactKey(row);
}
function rowTime(row: GalleryRow): number {
	return row.updatedAt ? Date.parse(row.updatedAt) || 0 : 0;
}
export function newestGalleryFirst(a: GalleryRow, b: GalleryRow): number {
	return rowTime(b) - rowTime(a) || galleryArtifactKey(a).localeCompare(galleryArtifactKey(b));
}
function groupKey(connection: DocumentConnection, projectId: string): string {
	return JSON.stringify([connection.kind, connection.identity, projectId]);
}
export function catalogArtifactGroup(
	workspace: WorkspaceSummary,
	connection: DocumentConnection,
	items: readonly CatalogItem[],
): GalleryGroup {
	return {
		id: groupKey(connection, workspace.id),
		name: workspace.name,
		isScratchpad: false,
		cloud: false,
		rows: items
			.map((item): CatalogArtifactRow => {
				const target = item.target;
				return {
					...item,
					connection,
					...(target.kind === "artifact"
						? {
								sessionTitle:
									workspace.sessions.find((session) => session.id === target.sessionId)?.title ?? target.sessionId,
							}
						: {}),
				};
			})
			.sort(newestGalleryFirst),
	};
}
/** Cloud and Scratchpad retain their existing file/read paths, never a local project catalog. */
export function legacyGalleryGroups(
	workspaces: readonly WorkspaceSummary[],
	connection: DocumentConnection,
): GalleryGroup[] {
	return groupArtifactsByProject(
		workspaces.filter(
			(workspace) => !workspace.hostId && (workspace.kind === "cloud" || workspace.id === STANDALONE_WORKSPACE_ID),
		),
	).map((group) => {
		const source: DocumentConnection = group.cloud
			? {
					kind: "cloud",
					identity:
						workspaces.find((workspace) => workspace.id === group.id && workspace.kind === "cloud")?.cloudOrgId ??
						group.id,
				}
			: connection;
		return {
			...group,
			id: groupKey(source, group.id),
			rows: group.rows.map((row) => ({ ...row, connection: source })),
		};
	});
}
export function filterGalleryGroups(
	groups: readonly GalleryGroup[],
	filters: GalleryFilters,
	scratchpadName?: string,
): GalleryGroup[] {
	const needle = filters.query.trim().toLocaleLowerCase();
	return groups
		.filter((group) => !filters.projectId || group.id === filters.projectId)
		.map((group) => ({
			...group,
			rows: group.rows.filter((row) => {
				const catalog = isCatalogArtifact(row);
				const source = catalog ? row.source : "worker";
				const archived = catalog && row.archived;
				if (filters.source !== "all" && source !== filters.source) return false;
				if ((filters.archive === "archive" && !archived) || (filters.archive === "current" && archived)) return false;
				const owner = catalog ? (row.target.kind === "artifact" ? row.target.sessionId : "") : row.sessionId;
				return (
					!needle ||
					[
						group.name,
						group.isScratchpad ? (scratchpadName ?? "") : "",
						row.name,
						row.path,
						row.sessionTitle ?? "",
						owner,
						source,
					].some((value) => value.toLocaleLowerCase().includes(needle))
				);
			}),
		}))
		.filter((group) => group.rows.length > 0);
}
/** Keep v1 keys for rollback; never guess ambiguous owners or remap a pin to a new connection. */
export function migrateLegacyPins(
	pins: readonly string[],
	rows: readonly GalleryRow[],
	aliases: Readonly<Record<string, string>> = {},
) {
	const next = [...pins],
		mapped = { ...aliases };
	for (const pin of pins) {
		if (pin.startsWith('["artifact-v2",') || mapped[pin]) continue;
		const matches = [...new Set(rows.filter((row) => galleryLegacyKey(row) === pin).map(galleryArtifactKey))];
		if (matches.length !== 1) continue;
		mapped[pin] = matches[0];
		if (!next.includes(matches[0])) next.push(matches[0]);
	}
	return { pins: next, aliases: mapped };
}
