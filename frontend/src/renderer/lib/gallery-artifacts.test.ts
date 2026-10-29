import { describe, expect, it } from "vitest";
import type { components } from "../../api/schema";
import type { WorkspaceSummary } from "../types/workspace";
import {
	catalogArtifactGroup,
	filterGalleryGroups,
	galleryArtifactKey,
	migrateLegacyPins,
	migratableGalleryRows,
} from "./gallery-artifacts";

const connection = { kind: "local", identity: "daemon-a" } as const;
const workspace: WorkspaceSummary = { id: "next.js", name: "Next project", path: "/registered", sessions: [] };
type Item = components["schemas"]["DocumentCatalogItem"];
const shared = (path = "same.md"): Item => ({
	source: "shared",
	archived: false,
	target: { kind: "shared", projectId: workspace.id, path },
	path,
	name: path.split("/").at(-1)!,
	size: 1,
	fingerprint: path,
});
const artifact = (source: "worker" | "orchestrator", archived = false): Item => ({
	...shared(),
	source,
	archived,
	target: { kind: "artifact", projectId: workspace.id, sessionId: source + "-1", path: "same.md" },
	updatedAt: "2026-10-29T10:00:00Z",
});
describe("managed gallery rows", () => {
	it("keeps worker, replaced orchestrator and independent shared sources, with unknown times last", () => {
		const group = catalogArtifactGroup(workspace, connection, [
			shared(),
			artifact("orchestrator", true),
			artifact("worker"),
		]);
		expect(group.rows).toHaveLength(3);
		expect(group.rows.at(-1)?.updatedAt).toBeUndefined();
		expect(new Set(group.rows.map(galleryArtifactKey)).size).toBe(3);
		expect(group.rows.find((row) => "target" in row && row.target.kind === "shared")).not.toHaveProperty("sessionId");
	});
	it("does not collide for equal paths across projects and connections", () => {
		const a = catalogArtifactGroup(workspace, connection, [shared()]).rows[0];
		const b = catalogArtifactGroup({ ...workspace, id: "other" }, connection, [
			{ ...shared(), target: { kind: "shared", projectId: "other", path: "same.md" } },
		]).rows[0];
		const c = catalogArtifactGroup(workspace, { kind: "local", identity: "daemon-b" }, [shared()]).rows[0];
		expect(new Set([a, b, c].map(galleryArtifactKey)).size).toBe(3);
	});
	it("defaults to all archives and filters source, path, project and source session ID", () => {
		const group = catalogArtifactGroup(workspace, connection, [
			shared("handoff/same.md"),
			artifact("orchestrator", true),
			artifact("worker"),
		]);
		const filters = { query: "", projectId: "", source: "all", archive: "all" } as const;
		expect(filterGalleryGroups([group], filters)[0].rows).toHaveLength(3);
		expect(filterGalleryGroups([group], { ...filters, archive: "archive" })[0].rows).toHaveLength(1);
		expect(filterGalleryGroups([group], { ...filters, archive: "current" })[0].rows).toHaveLength(2);
		expect(filterGalleryGroups([group], { ...filters, query: "handoff/", source: "shared" })[0].rows).toHaveLength(1);
		expect(filterGalleryGroups([group], { ...filters, query: "orchestrator-1" })[0].rows).toHaveLength(1);
		expect(filterGalleryGroups([group], { ...filters, query: "Next project" })[0].rows).toHaveLength(3);
		expect(filterGalleryGroups([group], { ...filters, projectId: "missing" })).toEqual([]);
	});
	it("migrates an unambiguous v1 session pin while retaining it for rollback", () => {
		const rows = catalogArtifactGroup(workspace, connection, [artifact("worker"), shared()]).rows;
		const legacy = JSON.stringify(["worker-1", "same.md"]);
		const migrated = migrateLegacyPins([legacy, "unavailable-old-pin"], rows);
		expect(migrated.pins).toEqual(expect.arrayContaining([legacy, galleryArtifactKey(rows[0]), "unavailable-old-pin"]));
		expect(migrateLegacyPins(migrated.pins, rows, migrated.aliases)).toEqual(migrated);
		const other = catalogArtifactGroup(workspace, { kind: "local", identity: "daemon-b" }, [artifact("worker")]).rows;
		expect(migrateLegacyPins(migrated.pins, other, migrated.aliases)).toEqual(migrated);
	});
	it("never guesses which project an ambiguous legacy pin belonged to", () => {
		const first = catalogArtifactGroup(workspace, connection, [artifact("worker")]).rows;
		const second = catalogArtifactGroup({ ...workspace, id: "other" }, connection, [
			{
				...artifact("worker"),
				target: { kind: "artifact", projectId: "other", sessionId: "worker-1", path: "same.md" },
			},
		]).rows;
		const legacy = JSON.stringify(["worker-1", "same.md"]);
		expect(migrateLegacyPins([legacy], [...first, ...second])).toEqual({ pins: [legacy], aliases: {} });
	});
});

it.each(["loading", "request", "changed", "owner", "unknown"])(
	"does not migrate an incomplete candidate: %s",
	(state) => {
		const rows = catalogArtifactGroup(workspace, connection, [artifact("worker")]).rows;
		const catalog = {
			projectId: workspace.id,
			items: [artifact("worker")],
			errors:
				state === "owner"
					? [{ source: "worker" as const, sessionId: "worker-1", code: "DOCUMENT_SOURCE_UNAVAILABLE" }]
					: [],
			loading: state === "loading",
			incomplete: state === "unknown" || state === "owner",
			error: state === "request" || state === "changed" ? new Error(state) : undefined,
		};
		expect(migratableGalleryRows(rows, [catalog])).toEqual([]);
	},
);
it("keeps an ambiguous legacy key unmigrated even if only one project is healthy", () => {
	const rows = [
		...catalogArtifactGroup(workspace, connection, [artifact("worker")]).rows,
		...catalogArtifactGroup({ ...workspace, id: "other" }, connection, [
			{
				...artifact("worker"),
				target: { kind: "artifact", projectId: "other", sessionId: "worker-1", path: "same.md" },
			},
		]).rows,
	];
	expect(
		migratableGalleryRows(rows, [
			{ projectId: workspace.id, items: [artifact("worker")], errors: [], loading: false, incomplete: false },
		]),
	).toEqual([]);
});
