import { create } from "zustand";
import { migrateLegacyPins, type GalleryFilters, type GalleryRow } from "../lib/gallery-artifacts";

export type ArtifactViewMode = "gallery" | "list";
export type GalleryPreferences = GalleryFilters & { scrollTop: number; visibleCount: number };
export const defaultGalleryPreferences: GalleryPreferences = {
	query: "",
	projectId: "",
	source: "all",
	archive: "all",
	scrollTop: 0,
	visibleCount: 200,
};
type ArtifactViewPreferences = {
	view: ArtifactViewMode;
	collapsedSections: string[];
	pinnedArtifactIds: string[];
	pinAliases: Record<string, string>;
	galleryPreferences: Record<string, GalleryPreferences>;
};
const STORAGE_KEY = "ao.artifacts.view";
const record = (value: unknown): value is Record<string, unknown> =>
	typeof value === "object" && value !== null && !Array.isArray(value);
const strings = (value: unknown): string[] =>
	Array.isArray(value) ? value.filter((id): id is string => typeof id === "string") : [];
function readContext(value: unknown): GalleryPreferences {
	if (!record(value)) return { ...defaultGalleryPreferences };
	return {
		query: typeof value.query === "string" ? value.query : "",
		projectId: typeof value.projectId === "string" ? value.projectId : "",
		source:
			value.source === "worker" || value.source === "orchestrator" || value.source === "shared" ? value.source : "all",
		archive: value.archive === "current" || value.archive === "archive" ? value.archive : "all",
		scrollTop:
			typeof value.scrollTop === "number" && Number.isFinite(value.scrollTop) && value.scrollTop >= 0
				? value.scrollTop
				: 0,
		visibleCount:
			typeof value.visibleCount === "number" && Number.isInteger(value.visibleCount) && value.visibleCount >= 200
				? value.visibleCount
				: 200,
	};
}
function readPreferences(storage: Storage): ArtifactViewPreferences {
	try {
		const saved: unknown = JSON.parse(storage.getItem(STORAGE_KEY) ?? "null");
		if (record(saved))
			return {
				view: saved.view === "list" ? "list" : "gallery",
				collapsedSections: strings(saved.collapsedSections),
				pinnedArtifactIds: strings(saved.pinnedArtifactIds),
				pinAliases: record(saved.pinAliases)
					? Object.fromEntries(
							Object.entries(saved.pinAliases).filter(
								(entry): entry is [string, string] =>
									typeof entry[1] === "string" && entry[1].startsWith('["artifact-v2",'),
							),
						)
					: {},
				galleryPreferences: record(saved.galleryPreferences)
					? Object.fromEntries(
							Object.entries(saved.galleryPreferences).map(([key, value]) => [key, readContext(value)]),
						)
					: {},
			};
	} catch {
		/* preferences remain available in memory */
	}
	return { view: "gallery", collapsedSections: [], pinnedArtifactIds: [], pinAliases: {}, galleryPreferences: {} };
}
function savePreferences(storage: Storage, preferences: ArtifactViewPreferences) {
	try {
		const { view, collapsedSections, pinnedArtifactIds, pinAliases, galleryPreferences } = preferences;
		storage.setItem(
			STORAGE_KEY,
			JSON.stringify({ pinVersion: 2, view, collapsedSections, pinnedArtifactIds, pinAliases, galleryPreferences }),
		);
	} catch {
		/* storage may be unavailable */
	}
}
type ArtifactsViewState = ArtifactViewPreferences & {
	setView: (view: ArtifactViewMode) => void;
	toggleSection: (sectionId: string) => void;
	togglePinnedArtifact: (artifactId: string, legacyId?: string) => void;
	unpinArtifact: (artifactId: string) => void;
	migratePins: (rows: readonly GalleryRow[]) => void;
	setGalleryPreferences: (connection: string, change: Partial<GalleryPreferences>) => void;
};
function pinFamily(id: string, aliases: Readonly<Record<string, string>>) {
	const canonical = aliases[id] ?? id;
	return [...new Set([id, canonical, ...Object.keys(aliases).filter((key) => aliases[key] === canonical)])];
}
export function createArtifactsViewStore(storage: Storage) {
	return create<ArtifactsViewState>((set) => {
		const change = (update: (state: ArtifactsViewState) => Partial<ArtifactViewPreferences>) =>
			set((state) => {
				const patch = update(state);
				savePreferences(storage, { ...state, ...patch });
				return patch;
			});
		return {
			...readPreferences(storage),
			setView: (view) => change(() => ({ view })),
			toggleSection: (id) =>
				change((state) => ({
					collapsedSections: state.collapsedSections.includes(id)
						? state.collapsedSections.filter((key) => key !== id)
						: [...state.collapsedSections, id],
				})),
			togglePinnedArtifact: (id, legacyId) =>
				change((state) => {
					const aliases = { ...state.pinAliases };
					if (legacyId && (!aliases[legacyId] || aliases[legacyId] === id)) aliases[legacyId] = id;
					const family = pinFamily(id, aliases);
					return {
						pinAliases: aliases,
						pinnedArtifactIds: family.some((key) => state.pinnedArtifactIds.includes(key))
							? state.pinnedArtifactIds.filter((key) => !family.includes(key))
							: [...family, ...state.pinnedArtifactIds],
					};
				}),
			unpinArtifact: (id) =>
				change((state) => ({
					pinnedArtifactIds: state.pinnedArtifactIds.filter((key) => !pinFamily(id, state.pinAliases).includes(key)),
				})),
			migratePins: (rows) =>
				change((state) => {
					const result = migrateLegacyPins(state.pinnedArtifactIds, rows, state.pinAliases);
					if (
						result.pins.length === state.pinnedArtifactIds.length &&
						Object.keys(result.aliases).length === Object.keys(state.pinAliases).length
					)
						return {};
					return { pinnedArtifactIds: result.pins, pinAliases: result.aliases };
				}),
			setGalleryPreferences: (connection, value) =>
				change((state) => ({
					galleryPreferences: {
						...state.galleryPreferences,
						[connection]: { ...defaultGalleryPreferences, ...state.galleryPreferences[connection], ...value },
					},
				})),
		};
	});
}
export const useArtifactsViewStore = createArtifactsViewStore(window.localStorage);
