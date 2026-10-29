import { beforeEach, expect, it, vi } from "vitest";
import { artifactKey } from "../lib/artifacts";
import { createArtifactsViewStore, useArtifactsViewStore } from "./artifacts-view-store";
import { catalogArtifactGroup, galleryArtifactKey } from "../lib/gallery-artifacts";

beforeEach(() => {
	window.localStorage.removeItem("ao.artifacts.view");
	useArtifactsViewStore.setState({ view: "gallery", collapsedSections: [], pinnedArtifactIds: [] });
});

it("unpins idempotently after a pin changes during deletion", () => {
	const id = artifactKey({ sessionId: "ao-1", path: "report.md" });
	const store = useArtifactsViewStore.getState();
	store.togglePinnedArtifact(id);
	store.togglePinnedArtifact(id);
	store.unpinArtifact(id);
	expect(useArtifactsViewStore.getState().pinnedArtifactIds).toEqual([]);
	expect(JSON.parse(window.localStorage.getItem("ao.artifacts.view") ?? "{}").pinnedArtifactIds).toEqual([]);
});

it("removes only the deleted artifact's pin and preserves view preferences", () => {
	const deleted = artifactKey({ sessionId: "ao-1", path: "report.md" });
	const kept = artifactKey({ sessionId: "ao-2", path: "report.md" });
	const store = useArtifactsViewStore.getState();
	store.setView("list");
	store.toggleSection("project:ao");
	store.togglePinnedArtifact(deleted);
	store.togglePinnedArtifact(kept);
	store.unpinArtifact(deleted);
	expect(useArtifactsViewStore.getState()).toMatchObject({
		view: "list",
		collapsedSections: ["project:ao"],
		pinnedArtifactIds: [kept],
	});
});

it("persists filters and scroll independently per connection and restores on reload", () => {
	const store = createArtifactsViewStore(window.localStorage);
	store
		.getState()
		.setGalleryPreferences("daemon-a", {
			query: "handoff",
			projectId: "p",
			source: "shared",
			archive: "all",
			scrollTop: 580,
			visibleCount: 600,
		});
	store.getState().setGalleryPreferences("daemon-b", { source: "orchestrator", archive: "archive" });
	store.getState().setView("list");
	const restored = createArtifactsViewStore(window.localStorage).getState();
	expect(restored.galleryPreferences["daemon-a"]).toMatchObject({
		query: "handoff",
		source: "shared",
		scrollTop: 580,
		visibleCount: 600,
	});
	expect(restored.galleryPreferences["daemon-b"]).toMatchObject({
		query: "",
		source: "orchestrator",
		archive: "archive",
		scrollTop: 0,
	});
	expect(restored.view).toBe("list");
});

it("migrates old worker pins without losing rollback keys and unpins both aliases", () => {
	const legacy = artifactKey({ sessionId: "worker", path: "same.md" });
	window.localStorage.setItem("ao.artifacts.view", JSON.stringify({ pinnedArtifactIds: [legacy], view: "list" }));
	const rows = catalogArtifactGroup(
		{ id: "p", name: "P", path: "/registered", sessions: [] },
		{ kind: "local", identity: "a" },
		[
			{
				source: "worker",
				target: { kind: "artifact", projectId: "p", sessionId: "worker", path: "same.md" },
				archived: false,
				name: "same.md",
				path: "same.md",
				size: 1,
				fingerprint: "v",
			},
		],
	).rows;
	const store = createArtifactsViewStore(window.localStorage);
	store.getState().migratePins(rows);
	expect(store.getState().pinnedArtifactIds).toEqual(expect.arrayContaining([legacy, galleryArtifactKey(rows[0])]));
	const restored = createArtifactsViewStore(window.localStorage);
	restored.getState().togglePinnedArtifact(galleryArtifactKey(rows[0]));
	expect(restored.getState().pinnedArtifactIds).toEqual([]);
	expect(restored.getState().view).toBe("list");
});

it("ignores malformed persisted filters instead of inventing a valid source", () => {
	window.localStorage.setItem(
		"ao.artifacts.view",
		JSON.stringify({
			galleryPreferences: { bad: { source: "workspace", archive: "dead", scrollTop: -1, visibleCount: -5 } },
		}),
	);
	expect(createArtifactsViewStore(window.localStorage).getState().galleryPreferences.bad).toMatchObject({
		source: "all",
		archive: "all",
		scrollTop: 0,
		visibleCount: 200,
	});
});

it("loads and remains usable when the localStorage getter throws", async () => {
	const getter = vi.spyOn(window, "localStorage", "get").mockImplementation(() => {
		throw new DOMException("Blocked", "SecurityError");
	});
	try {
		vi.resetModules();
		const { useArtifactsViewStore: isolated } = await import("./artifacts-view-store");
		isolated.getState().setView("list");
		isolated.getState().togglePinnedArtifact("memory-pin");
		expect(isolated.getState()).toMatchObject({ view: "list", pinnedArtifactIds: ["memory-pin"] });
	} finally {
		getter.mockRestore();
	}
});
