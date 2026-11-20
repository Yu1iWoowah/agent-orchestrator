import { keptArtifactsQueryKey, useKeptArtifacts } from "../hooks/useKeptArtifacts";
import { FilterChip, ToolbarSearch } from "./PageToolbar";
import { hidesShellTopbar } from "../lib/platform";
import {
	ChevronDown, Filter, SelectMultiple,
	File,
	LayoutGrid,
	List,
	Loader2,
	Pin,
	Plus,
	RefreshCw,
} from "@/components/app-icons";
import { ScrollArea } from "radix-ui";
import { memo, useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { artifactFileIcon } from "./artifact-file-icon";
import { useProjectAvatars } from "../hooks/useProjectAvatars";
import { ProjectIcon } from "./ProjectIcon";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useCloudCp } from "../hooks/useCloudCp";
import { useCloudMeUserId } from "../hooks/useCloudMeUserId";
import { useTranslation } from "react-i18next";
import { isCatalogIncomplete, isUninitializedSharedSource } from "../lib/artifact-catalog";
import { useShallow } from "zustand/react/shallow";
import { DocumentReadError } from "../lib/document-read";
import { useCloudOffering } from "../hooks/useCloudOffering";
import { useCreateArtifactSession } from "../hooks/useCreateArtifactSession";
import {
	catalogArtifactGroup,
	filterGalleryGroups,
	galleryArtifactKey,
	galleryLegacyKey,
	isCatalogArtifact,
	legacyGalleryGroups,
	withKeptGalleryGroups,
	newestGalleryFirst,
	migratableGalleryRows,
	type GalleryRow,
} from "../lib/gallery-artifacts";
import { useArtifactCatalogs } from "../hooks/useArtifactCatalogs";
import {
	DocumentReadingContext,
	DocumentReadingProvider,
	openReadingDocument,
	useDocumentReader,
} from "../lib/document-reading-context";
import { DocumentSheet } from "./chat/DocumentSheet";
import { CatalogArtifactCard } from "./CatalogArtifactCard";
import { deleteSessionArtifactFile } from "../hooks/useSessionWorkspaceFiles";
import { cloudSessionsQueryKey, useWorkspaceQuery, workspaceQueryKeyForHost } from "../hooks/useWorkspaceQuery";
import {
	ARTIFACT_SHARINGS,
	ARTIFACT_TYPES,
	artifactChangeError,
	artifactContext,
	withKeptArtifacts,
	canChangeSessionArtifacts,
	groupArtifactsByProject,
	withShareAccess, artifactType, artifactSharing,
	type ArtifactLocation,
	type ArtifactRow,
	type ArtifactSharing,
	type ArtifactType,
} from "../lib/artifacts";
import { formatTimeCompact } from "../lib/format-time";
import { useNavigateToSession } from "../lib/navigate-to-session";
import { cn } from "../lib/utils";
import {
	defaultGalleryPreferences,
	useArtifactsViewStore,
	type ArtifactViewMode,
	type GalleryPreferences,
} from "../stores/artifacts-view-store";
import { useUiStore } from "../stores/ui-store";
import { CLOUD_PROJECT_KIND, STANDALONE_WORKSPACE_ID, type WorkspaceSummary } from "../types/workspace";
import { DeferredArtifactActionsMenu } from "./ArtifactActionsMenu";
import { useArtifactShareFile, useReconciledShareLinks } from "./ShareArtifactPopover";
import { ArtifactGalleryCard, type ArtifactCardSelection } from "./ArtifactGalleryCard";
import { ArtifactShareMark } from "./ArtifactShareMark";
import { ConfirmDialog } from "./ConfirmDialog";
import { Checkbox } from "./ui/checkbox";
import { ArtifactPreviewModal } from "./ArtifactPreviewModal";
import { Button } from "./ui/button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuRadioGroup,
	DropdownMenuRadioItem,
	DropdownMenuLabel,
	DropdownMenuCheckboxItem,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "./ui/dropdown-menu";
import { Pressable } from "./ui/pressable";
import { Tooltip, TooltipContent, TooltipTrigger } from "./ui/tooltip";

// Three previews a row; narrow panels drop to two, then one.
const ARTIFACT_GRID_CLASS = "grid grid-cols-1 gap-4 @lg:grid-cols-2 @2xl:grid-cols-3";
/** How many selected names the bulk delete confirmation lists before "+N more". */
const MAX_CONFIRM_NAMES = 5;

/**
 * A project's icon as the sidebar draws it: its repository owner's avatar or
 * colour-coded letter tile, or a folder with avatars off.
 */
function ArtifactProjectIcon({ workspace, avatarUrl }: { workspace: Pick<WorkspaceSummary, "id" | "name" | "path">; avatarUrl?: string }) {
	return (
		<span aria-hidden="true" className="inline-flex size-icon-md shrink-0 items-center justify-center">
			<ProjectIcon avatarUrl={avatarUrl} expanded={false} name={workspace.name} seed={workspace.path || workspace.id} />
		</span>
	);
}

/**
 * Every artifact agents have written, grouped by project: on this machine, and
 * in cloud sessions when the control plane keeps artifacts.
 */
export function ArtifactsView() {
	return (
		<DocumentReadingProvider scope="gallery">
			<ArtifactsGallery />
		</DocumentReadingProvider>
	);
}
function ArtifactsGallery() {
	const { t } = useTranslation();
	const reader = useDocumentReader();
	const surface = useRef<HTMLDivElement>(null),
		viewport = useRef<HTMLDivElement>(null);
	const restored = useRef<string | null>(null);
	const showPageTitle = hidesShellTopbar();
	const cloudArtifacts = useCloudOffering().artifacts;
	const workspaceQuery = useWorkspaceQuery({ includeCloud: cloudArtifacts });
	const workspaces = workspaceQuery.data;
	const kept = useKeptArtifacts().data;
	const noProjectName = t("artifacts.noProject");
	const catalog = useArtifactCatalogs(workspaces);
	const { query, projectId, source, archive, visibleCount } = useArtifactsViewStore(
		useShallow((state) => {
			const { query, projectId, source, archive, visibleCount } =
				state.galleryPreferences[catalog.contextKey] ?? defaultGalleryPreferences;
			return { query, projectId, source, archive, visibleCount };
		}),
	);
	const setPreferences = useArtifactsViewStore((state) => state.setGalleryPreferences);
	const pendingScroll = useRef<{ key: string; top: number } | null>(null);
	const scrollTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
	const flushScroll = useCallback(() => {
		clearTimeout(scrollTimer.current);
		scrollTimer.current = undefined;
		const pending = pendingScroll.current;
		pendingScroll.current = null;
		if (pending) setPreferences(pending.key, { scrollTop: pending.top });
	}, [setPreferences]);
	useEffect(() => {
		window.addEventListener("pagehide", flushScroll);
		return () => {
			window.removeEventListener("pagehide", flushScroll);
			flushScroll();
		};
	}, [catalog.contextKey, flushScroll]);
	const view = useArtifactsViewStore((state) => state.view),
		setView = useArtifactsViewStore((state) => state.setView);
	const pins = useArtifactsViewStore((state) => state.pinnedArtifactIds);
	const [preview, setPreview] = useState<{
		row: ArtifactRow & { connection: GalleryRow["connection"] };
		projectName: string;
	} | null>(null);
    const shareAccess = useReconciledShareLinks();
    const publishedRows = useMemo(() => withShareAccess(groupArtifactsByProject(workspaces ?? [], t("artifacts.source.orchestrator")), shareAccess).flatMap((group) => group.rows), [workspaces, shareAccess, t]);
	const groups = useMemo(
		() =>
			withKeptGalleryGroups([
				...catalog.projects.map((project) =>
					catalogArtifactGroup(
						project,
						catalog.connection,
						catalog.data?.find((entry) => entry.projectId === project.id)?.items ?? [],
					),
				),
				...legacyGalleryGroups(workspaces ?? [], catalog.connection),
			], catalog.connection, withKeptArtifacts([], kept, workspaces ?? [], noProjectName)).map((group) => ({ ...group, rows: group.rows.flatMap((row) => {
                if (!isCatalogArtifact(row) || row.connection.kind !== "local" || row.target.kind !== "artifact") return [row];
                const sessionId = row.target.sessionId;
                const published = publishedRows.find((entry) => entry.sessionId === sessionId && (entry.path === row.path || (entry.bundle && row.path.startsWith(entry.bundle.path + "/"))));
                if (published?.bundle && published.path !== row.path) return [];
                return [{ ...row, name: published?.name ?? row.name, sessionTitle: published?.sessionTitle ?? row.sessionTitle }];
            }) })).sort((a, b) =>
				a.rows[0] && b.rows[0]
					? newestGalleryFirst(a.rows[0], b.rows[0])
					: a.rows.length
						? -1
						: b.rows.length
							? 1
							: a.id.localeCompare(b.id),
			),
		[catalog.projects, catalog.connection, catalog.data, workspaces, publishedRows, kept, noProjectName],
	);
	const allRows = useMemo(() => groups.flatMap((group) => group.rows), [groups]);
	const complete = catalog.data !== undefined && catalog.data.every((entry) => !entry.loading);
	const incomplete = !complete || Boolean(catalog.data?.some(isCatalogIncomplete));
	const migrationRows = useMemo(() => migratableGalleryRows(allRows, catalog.data), [allRows, catalog.data]);
	useEffect(() => {
		useArtifactsViewStore.getState().migratePins(migrationRows);
	}, [migrationRows]);
	const legacyPins = useMemo(
		() => new Map(migrationRows.map((row) => [galleryLegacyKey(row), galleryArtifactKey(row)])),
		[migrationRows],
	);
    const artifactFor = useCallback((row: GalleryRow): ArtifactRow | undefined => {
        if (!isCatalogArtifact(row)) return { ...row, shareAccess: row.shareUrl ? shareAccess.get(row.shareUrl) ?? row.shareAccess : row.shareAccess };
        const target = row.target;
        if (target.kind !== "artifact") return undefined;
        const known = row.connection.kind === "local" ? publishedRows.find((entry) => entry.sessionId === target.sessionId && entry.path === row.path) : undefined;
        if (known) return { ...known, updatedAt: row.updatedAt ?? known.updatedAt };
        return { name: row.name, path: row.path, kind: /\.html?$/i.test(row.path) ? "html" : /\.(md|markdown)$/i.test(row.path) ? "markdown" : "file", updatedAt: row.updatedAt ?? "", sessionId: target.sessionId, sessionTitle: row.sessionTitle ?? "", projectId: row.target.projectId };
    }, [publishedRows, shareAccess]);
    const [projectIds, setProjectIds] = useState<readonly string[]>(() => projectId ? [projectId] : []);
	useEffect(() => {
		const saved = useArtifactsViewStore.getState().galleryPreferences[catalog.contextKey]?.projectId;
		setProjectIds(saved ? [saved] : []);
	}, [catalog.contextKey]);
    const [location, setLocation] = useState<ArtifactLocation | undefined>();
    const [types, setTypes] = useState<readonly ArtifactType[]>([]);
    const [sharings, setSharings] = useState<readonly ArtifactSharing[]>([]);
	const scratchpadName = t("standalone.workspaceName");
	const visibleGroups = useMemo(
		() => filterGalleryGroups(groups, { query, projectId: "", source, archive }, scratchpadName)
            .filter((group) => projectIds.length === 0 || projectIds.includes(group.id))
            .map((group) => ({ ...group, rows: group.rows.filter((row) => {
                const artifact = artifactFor(row);
                const sharing = artifact ? artifactSharing(artifact) : "private";
                return (!location || (group.cloud ? "cloud" : "local") === location)
                    && (types.length === 0 || types.includes(artifactType(row)))
                    && (sharings.length === 0 || Boolean(sharing && sharings.includes(sharing)));
            }) })).filter((group) => group.rows.length > 0),
		[groups, query, source, archive, scratchpadName, projectIds, location, types, sharings, artifactFor],
	);
	const cards = useMemo(
		() =>
			visibleGroups
				.flatMap((group) =>
					group.rows.map((row) => ({ row, projectName: group.isScratchpad ? scratchpadName : group.name })),
				)
				.sort((a, b) => newestGalleryFirst(a.row, b.row)),
		[visibleGroups, scratchpadName],
	);
	const pinnedCards = useMemo(() => {
		const byId = new Map(cards.map((card) => [galleryArtifactKey(card.row), card]));
		return pins.flatMap((id) => {
			const card = byId.get(id);
			return card ? [card] : [];
		});
	}, [cards, pins]);
	const pinnedKeys = new Set(pinnedCards.map(({ row }) => galleryArtifactKey(row)));
	const orderedCards = [...pinnedCards, ...cards.filter(({ row }) => !pinnedKeys.has(galleryArtifactKey(row)))];
    const livePreviews = new Set(orderedCards.filter(({ row }) => {
        const artifact = artifactFor(row);
        return artifact && ((artifact.kind === "html" && (artifact.cloud || (artifact.inlineUrl && artifact.rawUrl))) || (/\.pdf$/i.test(row.path) && (artifact.cloud || artifact.rawUrl)));
    }).slice(0, 4).map(({ row }) => galleryArtifactKey(row)));
    const previewRow = preview ? { ...preview, row: { ...(publishedRows.find((row) => row.sessionId === preview.row.sessionId && row.path === preview.row.path) ?? preview.row), connection: preview.row.connection } } : null;
	const openPreview = (row: GalleryRow, projectName: string) => {
		if (isCatalogArtifact(row)) {
			if (reader) openReadingDocument(reader, row.target);
		} else setPreview({ row, projectName });
	};
	const queryClient = useQueryClient();
	const { client: cloudClient } = useCloudCp(cloudArtifacts);
	const cloudUserId = useCloudMeUserId(cloudArtifacts);
	// Another member's cloud session files are view only, so they cannot be selected.
	// A bundle row stands for one of its files, so deleting it would leave the
	// rest of the bundle behind; it has no Delete in its menu either.
    const deleteBlockedReason = (row: GalleryRow) => {
        const artifact = artifactFor(row);
        if (!artifact || (isCatalogArtifact(row) && row.connection.kind !== "local")) return t("artifacts.viewOnly");
        if (artifact.bundle) return t("artifacts.bundleNotDeletable");
        return canChangeSessionArtifacts(artifact.cloud, cloudUserId) ? undefined : t("artifacts.viewOnly");
    };
	const [selecting, setSelecting] = useState(false);
	const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(() => new Set());
	const [confirmingBulkDelete, setConfirmingBulkDelete] = useState(false);
	// A selected file deleted elsewhere leaves the selection as the list
	// refreshes, so a later file at the same path starts unselected. The
	// check runs against every artifact, not the filtered cards: the
	// selection outlives the filters, so the confirmation names every file.
	useEffect(() => {
		const present = new Set(groups.flatMap((group) => group.rows.map(galleryArtifactKey)));
		setSelectedIds((current) => {
			const kept = [...current].filter((id) => present.has(id));
			return kept.length === current.size ? current : new Set(kept);
		});
	}, [groups]);
	const selectedRows = useMemo(
		() => groups.flatMap((group) => group.rows).filter((row) => selectedIds.has(galleryArtifactKey(row))),
		[groups, selectedIds],
	);
	const selectableVisibleIds = cards.filter(({ row }) => !deleteBlockedReason(row)).map(({ row }) => galleryArtifactKey(row));
	const allVisibleSelected = selectableVisibleIds.length > 0 && selectableVisibleIds.every((id) => selectedIds.has(id));
	const toggleSelected = (id: string) => setSelectedIds((current) => {
		const next = new Set(current);
		if (!next.delete(id)) next.add(id);
		return next;
	});
	const toggleAllVisible = () => setSelectedIds((current) => {
		const next = new Set(current);
		for (const id of selectableVisibleIds) {
			if (allVisibleSelected) next.delete(id);
			else next.add(id);
		}
		return next;
	});
	const stopSelecting = () => {
		setSelecting(false);
		setSelectedIds(new Set());
	};
	// Each file is deleted on its own; the ones that fail stay selected so
	// the person can retry them.
	const bulkDelete = useMutation({
		mutationFn: async (rows: readonly GalleryRow[]) => {
            const results = await Promise.allSettled(rows.map((row) => {
                const blocked = deleteBlockedReason(row);
                const artifact = artifactFor(row);
                if (blocked || !artifact) return Promise.reject(new Error(blocked ?? t("artifacts.viewOnly")));
                return artifact.cloud
                    ? cloudClient.deleteSessionArtifact(artifact.cloud.orgId, artifact.sessionId, artifact.path).catch((error: unknown) => { throw artifactChangeError(error, t("artifacts.viewOnly")); })
                    : deleteSessionArtifactFile({ path: artifact.path, sessionId: artifact.sessionId });
            }));
			const deleted = rows.filter((_, index) => results[index]?.status === "fulfilled").map(galleryArtifactKey);
			const failures = results.flatMap((result) => result.status === "rejected" ? [result.reason] : []);
			const { unpinArtifact } = useArtifactsViewStore.getState();
			for (const id of deleted) unpinArtifact(id);
			setSelectedIds((current) => new Set([...current].filter((id) => !deleted.includes(id))));
			await Promise.all([
				queryClient.invalidateQueries({ queryKey: workspaceQueryKeyForHost() }),
				...(rows.some((row) => !isCatalogArtifact(row) && row.kept) ? [queryClient.invalidateQueries({ queryKey: keptArtifactsQueryKey })] : []),
                queryClient.invalidateQueries({ queryKey: ["artifact-catalogs", catalog.connection.kind, catalog.connection.identity] }),
				...(rows.some((row) => !isCatalogArtifact(row) && row.cloud) ? [queryClient.invalidateQueries({ queryKey: cloudSessionsQueryKey })] : []),
			]);
			if (failures.length > 0) {
				const reason = failures[0];
				throw new Error(t("artifacts.bulkDeleteFailed", {
					failed: failures.length,
					total: rows.length,
					message: reason instanceof Error ? reason.message : String(reason),
				}));
			}
		},
		onSuccess: () => {
			setConfirmingBulkDelete(false);
			stopSelecting();
		},
	});
	const confirmBulkDelete = () => {
		bulkDelete.reset();
		setConfirmingBulkDelete(true);
	};
	useEffect(() => {
		if (!selecting || confirmingBulkDelete) return;
		const onKeyDown = (event: KeyboardEvent) => {
			// Escape inside search or an open menu closes that first.
			if (event.key !== "Escape" || event.defaultPrevented) return;
			setSelecting(false);
			setSelectedIds(new Set());
		};
		window.addEventListener("keydown", onKeyDown);
		return () => window.removeEventListener("keydown", onKeyDown);
	}, [selecting, confirmingBulkDelete]);
	const selectionFor = (row: GalleryRow, projectName: string): ArtifactCardSelection | undefined => {
		if (!selecting) return undefined;
		const id = galleryArtifactKey(row);
		return {
			selected: selectedIds.has(id),
			disabledReason: deleteBlockedReason(row),
			label: t("artifacts.selectArtifact", { name: row.name }),
			description: `${projectName} · ${row.sessionTitle} · ${row.path}`,
			onToggle: () => toggleSelected(id),
		};
	};

	const card = (row: GalleryRow, projectName: string) => {
		const key = galleryArtifactKey(row),
			legacy = galleryLegacyKey(row);
		const alias = legacy && legacyPins.get(legacy) === key ? legacy : undefined;
		const target = isCatalogArtifact(row) ? row.target : undefined;
        const published = row.connection.kind === "local" && target?.kind === "artifact"
            ? publishedRows.find((entry) => entry.sessionId === target.sessionId && entry.path === row.path)
            : undefined;
        const rendered = published ?? (row as ArtifactRow);
        return isCatalogArtifact(row) && !published ? (
			<CatalogArtifactCard
				key={key}
				row={row}
				projectName={projectName}
				view={view}
				legacyPin={alias}
                selection={selectionFor(row, projectName)}
				onOpen={() => openPreview(row, projectName)}
			/>
		) : (
			<ArtifactCard
				key={key}
				row={{ ...rendered, connection: row.connection, shareAccess: rendered.shareUrl ? shareAccess.get(rendered.shareUrl) ?? rendered.shareAccess : undefined }}
                previewLive={livePreviews.has(key)}
                pinId={key}
				projectName={projectName}
				view={view}
				showProjectName
				legacyPin={alias}
                selection={selectionFor(row, projectName)}
				onPreview={published ? openPreview : () => openPreview(row, projectName)}
			/>
		);
	};
	const changeFilters = (change: Partial<GalleryPreferences>) => {
		flushScroll();
		restored.current = catalog.contextKey;
		setPreferences(catalog.contextKey, { ...change, scrollTop: 0, visibleCount: 200 });
		if (viewport.current) viewport.current.scrollTop = 0;
	};
	const selectProjects = (ids: readonly string[]) => {
		setProjectIds(ids);
		// The fork remembers a single project; U's multi-selection is view-local.
		changeFilters({ projectId: ids.length === 1 ? ids[0] : "" });
	};
	const clearFilters = () => { setProjectIds([]); setLocation(undefined); setTypes([]); setSharings([]); changeFilters({ query: "", projectId: "", source: "all", archive: "all" }); };
	useLayoutEffect(() => {
		if (restored.current === catalog.contextKey) return;
		const top = useArtifactsViewStore.getState().galleryPreferences[catalog.contextKey]?.scrollTop ?? 0;
		const frame = requestAnimationFrame(() => {
			const node = viewport.current;
			if (!node || (top > node.scrollHeight - node.clientHeight && catalog.isFetching)) return;
			node.scrollTop = top;
			restored.current = catalog.contextKey;
		});
		return () => cancelAnimationFrame(frame);
	}, [catalog.contextKey, catalog.isFetching, cards.length, visibleCount]);
	const createArtifact = useCreateArtifactSession();
	const sourceNames = {
		all: t("artifacts.allSources"),
		worker: t("artifacts.source.worker"),
		orchestrator: t("artifacts.source.orchestrator"),
		shared: t("artifacts.source.shared"),
	};
	const archiveNames = {
		all: t("artifacts.archive.all"),
		current: t("artifacts.archive.current"),
		archive: t("artifacts.archive.archive"),
	};
	const hasArtifacts = allRows.length > 0;
	const sourceProblems =
		catalog.data?.flatMap((entry) =>
			[
				...(entry.error
					? [
							{
								source: "all",
								code: entry.error instanceof DocumentReadError ? entry.error.code : "DOCUMENT_SOURCE_UNAVAILABLE",
								sessionId: undefined,
							},
						]
					: entry.errors.filter((error) => !isUninitializedSharedSource(error))),
			].map((error) => ({ ...error, projectId: entry.projectId })),
		) ?? [];
	return (
		<div ref={surface} className="relative flex min-h-0 flex-1 flex-col bg-background">
			<header className="workspace-topbar-container center-panel-titlebar flex h-toolbar shrink-0 items-center justify-between gap-3 border-b border-border-strong pr-[calc((var(--size-toolbar)-var(--size-topbar-control))/2)]">
				<h1 className={showPageTitle ? "inline-flex items-center gap-1.5 text-subtitle font-semibold" : "sr-only"}>
					{showPageTitle ? <File className="size-icon-md" aria-hidden="true" /> : null}
					{t("artifacts.title")}
				</h1>
				<div className="workspace-topbar-actions flex min-w-0 flex-1 items-center justify-end gap-2">
													<DropdownMenu>
									<Tooltip>
										<TooltipTrigger asChild>
											<DropdownMenuTrigger asChild>
												<Button type="button" size="icon-sm" variant="quiet" aria-label={t("artifacts.filter")} data-active={(projectIds.length || types.length || sharings.length) ? "true" : undefined}>
													<Filter aria-hidden="true" className="size-icon-sm" />
												</Button>
											</DropdownMenuTrigger>
										</TooltipTrigger>
										<TooltipContent side="bottom">{t("artifacts.filter")}</TooltipContent>
									</Tooltip>
									<DropdownMenuContent align="end" className="max-h-[min(36rem,80vh)] min-w-52 overflow-y-auto" data-browser-native-overlay="true">
										<DropdownMenuLabel>{t("artifacts.filterProject")}</DropdownMenuLabel>
										<FilterOption checked={projectIds.length === 0} onToggle={() => selectProjects([])}>{t("artifacts.allProjects")}</FilterOption>
										{groups
											.filter((group) => !location || (group.cloud ? "cloud" : "local") === location)
											.map((group) => (
												<FilterOption key={group.id} checked={projectIds.includes(group.id)} onToggle={() => selectProjects(toggled(projectIds, group.id))}>
													<span className="truncate">{(group.isScratchpad ? scratchpadName : group.name)}</span>
												</FilterOption>
											))}
										<DropdownMenuSeparator />
										<DropdownMenuLabel>{t("artifacts.filterType")}</DropdownMenuLabel>
										<FilterOption checked={types.length === 0} onToggle={() => setTypes([])}>{t("artifacts.allTypes")}</FilterOption>
										{ARTIFACT_TYPES.map((value) => (
											<FilterOption key={value} checked={types.includes(value)} onToggle={() => setTypes(toggled(types, value))}>{t(`artifacts.type.${value}`)}</FilterOption>
										))}
										<DropdownMenuSeparator />
										<DropdownMenuLabel>{t("artifacts.filterSharing")}</DropdownMenuLabel>
										{ARTIFACT_SHARINGS.map((value) => (
											<FilterOption key={value} checked={sharings.includes(value)} onToggle={() => setSharings(toggled(sharings, value))}>{t(`artifacts.sharing.${value}`)}</FilterOption>
										))}
										<FilterOption checked={sharings.length === 0} onToggle={() => setSharings([])}>{t("artifacts.allSharing")}</FilterOption>
									</DropdownMenuContent>
								</DropdownMenu>
<ToolbarSearch label={t("artifacts.search")} query={query} onQueryChange={(query) => changeFilters({ query })} />
					{!selecting && <><div role="group" aria-label={t("artifacts.view")} className="inline-flex shrink-0 items-center">
						{(
							[
								{ value: "gallery", label: t("artifacts.galleryView"), Icon: LayoutGrid },
								{ value: "list", label: t("artifacts.listView"), Icon: List },
							] as const
						).map(({ value, label, Icon }) => (
							<Tooltip key={value}>
								<TooltipTrigger asChild>
									<Button
										variant="quiet"
										size="icon-sm"
										aria-label={label}
										aria-pressed={view === value}
										onClick={() => setView(value)}
										className={cn(view === value && "bg-interactive-active")}
									>
										<Icon className="size-icon-sm" />
									</Button>
								</TooltipTrigger>
								<TooltipContent>{label}</TooltipContent>
							</Tooltip>
						))}
					</div>
					<CreateArtifactMenu
						workspaces={workspaces ?? []}
						create={createArtifact.create}
						pending={createArtifact.pending}
						label="new"
						align="end"
						localDisabledReason={createArtifact.agentReady ? undefined : t("artifacts.noAgent.description")}
					/></>}
				</div>
			</header>
			<div data-conversation-frame-avoid="" className="flex shrink-0 flex-wrap items-center gap-2 border-b px-4 py-2">
				<DropdownMenu>
					<DropdownMenuTrigger asChild>
						<Button variant="ghost" size="sm" aria-label={t("artifacts.sourceFilter")}>
							{sourceNames[source]}
							<ChevronDown className="size-4" />
						</Button>
					</DropdownMenuTrigger>
					<DropdownMenuContent>
						<DropdownMenuRadioGroup
							value={source}
							onValueChange={(value) => {
								if (value === "all" || value === "worker" || value === "orchestrator" || value === "shared")
									changeFilters({ source: value });
							}}
						>
							{Object.entries(sourceNames).map(([value, label]) => (
								<DropdownMenuRadioItem key={value} value={value}>
									{label}
								</DropdownMenuRadioItem>
							))}
						</DropdownMenuRadioGroup>
					</DropdownMenuContent>
				</DropdownMenu>
				<DropdownMenu>
					<DropdownMenuTrigger asChild>
						<Button variant="ghost" size="sm" aria-label={t("artifacts.archiveFilter")}>
							{t("artifacts.archiveFilter")}: {archiveNames[archive]}
							<ChevronDown className="size-4" />
						</Button>
					</DropdownMenuTrigger>
					<DropdownMenuContent>
						<DropdownMenuRadioGroup
							value={archive}
							onValueChange={(value) => {
								if (value === "all" || value === "current" || value === "archive") changeFilters({ archive: value });
							}}
						>
							{Object.entries(archiveNames).map(([value, label]) => (
								<DropdownMenuRadioItem key={value} value={value}>
									{label}
								</DropdownMenuRadioItem>
							))}
						</DropdownMenuRadioGroup>
					</DropdownMenuContent>
				</DropdownMenu>
				<Button variant="ghost" size="sm" disabled={catalog.isFetching} onClick={() => void catalog.refetch()}>
					<RefreshCw className="size-4" />
					{t("artifacts.refresh")}
				</Button>
				<span role="status" className="text-xs text-muted-foreground">
					{incomplete ? t("artifacts.totalUnknown") : t("artifacts.filesLoaded", { count: allRows.length })}
				</span>
				{incomplete && allRows.length > 0 ? (
					<span className="text-xs text-muted-foreground">{t("artifacts.filesLoaded", { count: allRows.length })}</span>
				) : null}
				<DocumentSheet surfaceRef={surface} />
			</div>
            <div className="flex flex-wrap items-center gap-2 px-4 py-2">
                {([
                    [t("artifacts.filterProject"), projectIds.map((id) => { const group = groups.find((entry) => entry.id === id); return group?.isScratchpad ? scratchpadName : group?.name ?? id; }), () => selectProjects([])],
                    [t("artifacts.filterType"), types.map((value) => t(`artifacts.type.${value}`)), () => setTypes([])],
                    [t("artifacts.filterSharing"), sharings.map((value) => t(`artifacts.sharing.${value}`)), () => setSharings([])],
                ] as const).map(([label, values, clear]) => values.length ? <FilterChip key={label} label={label} value={values.join(", ")} removeLabel={t("artifacts.removeFilter", { filter: `${label}: ${values.join(", ")}` })} onRemove={clear} /> : null)}
                <div role="group" aria-label={t("artifacts.location")}>
                    {([undefined, "local", ...(cloudArtifacts ? ["cloud"] : [])] as const).map((value) => <Button key={value ?? "all"} type="button" variant="ghost" size="sm" aria-pressed={location === value} onClick={() => setLocation(value as ArtifactLocation | undefined)}>{t(value === "local" ? "artifacts.locationLocal" : value === "cloud" ? "artifacts.locationCloud" : "artifacts.locationAll")}</Button>)}
                </div>
                {selecting ? <div role="group" aria-label={t("artifacts.selection")} className="flex items-center gap-2">
                    <span aria-live="polite">{t("artifacts.selectedCount", { count: selectedRows.length })}</span>
                    <Button type="button" size="sm" variant="quiet" disabled={!selectableVisibleIds.length} onClick={toggleAllVisible}>{t(allVisibleSelected ? "artifacts.deselectAll" : "artifacts.selectAll")}</Button>
                    <Button type="button" size="sm" variant="destructive" disabled={!selectedRows.length} onClick={confirmBulkDelete}>{t("artifacts.deleteSelected")}</Button>
                    <Button type="button" size="sm" variant="quiet" onClick={stopSelecting}>{t("artifacts.cancelSelection")}</Button>
                </div> : hasArtifacts ? <Button type="button" size="sm" variant="quiet" onClick={() => setSelecting(true)}><SelectMultiple aria-hidden="true" />{t("artifacts.select")}</Button> : null}
            </div>

			<ScrollArea.Root asChild type="hover" scrollHideDelay={500}>
				<main className="relative min-h-0 flex-1 overflow-hidden">
					<ScrollArea.Viewport
						ref={viewport}
						className="chat-scroll-viewport h-full w-full overscroll-contain focus-visible:outline-none"
						onWheelCapture={() => {
							restored.current = catalog.contextKey;
						}}
						onScroll={(event) => {
							if (restored.current === catalog.contextKey) {
								pendingScroll.current = { key: catalog.contextKey, top: event.currentTarget.scrollTop };
								if (scrollTimer.current === undefined) scrollTimer.current = setTimeout(flushScroll, 150);
							}
						}}
					>
						<div className="px-4 pb-4">
							{catalog.isFetching || workspaceQuery.isLoading ? (
								<p role="status" className="py-2 text-sm text-muted-foreground">
									{t("artifacts.loading")}
								</p>
							) : null}
							{workspaceQuery.error ? (
								<p role="alert" className="py-2 text-sm text-destructive">
									{workspaceQuery.error.message}
								</p>
							) : null}
							{source === "shared" &&
							catalog.data?.some((project) => project.errors.some(isUninitializedSharedSource)) ? (
								<p role="status" className="py-2 text-sm text-muted-foreground">
									{t("documents.sheet.notCreated")}
								</p>
							) : null}
							{sourceProblems.length > 0 ? (
								<div className="py-2 text-sm">
									<p>{t("artifacts.sourcesIncomplete")}</p>
									<ul>
										{sourceProblems.map((error, index) => (
											<li key={`${error.projectId}:${error.source}:${error.sessionId}:${index}`} role="alert">
												{catalog.projects.find((project) => project.id === error.projectId)?.name ?? error.projectId} ·{" "}
												{error.source === "worker" || error.source === "orchestrator" || error.source === "shared"
													? sourceNames[error.source]
													: sourceNames.all}{" "}
												{error.sessionId ?? ""}:{" "}
												{error.code === "DOCUMENT_CATALOG_CHANGED"
													? t("artifacts.catalogChanged")
													: t("documents.sheet.unavailable")}
											</li>
										))}
									</ul>
								</div>
							) : null}
							{source === "shared" && archive === "archive" ? (
								<p className="py-2 text-sm text-muted-foreground">{t("artifacts.sharedArchiveHint")}</p>
							) : null}
							{!hasArtifacts && !incomplete && !workspaceQuery.isLoading ? (
								<EmptyArtifacts workspaces={workspaces ?? []} />
							) : cards.length === 0 && !catalog.isFetching ? (
								<div className="py-8 text-center">
									<p>{t(incomplete ? "artifacts.noLoadedMatches" : "artifacts.noMatches")}</p>
									<Button variant="ghost" onClick={clearFilters}>
										{t("artifacts.clearFilters")}
									</Button>
								</div>
							) : (
                                <ul aria-label={t("artifacts.title")} className={view === "gallery" ? ARTIFACT_GRID_CLASS : "grid gap-2"}>
                                    {orderedCards.slice(0, visibleCount).map(({ row, projectName }) => card(row, projectName))}
                                </ul>
							)}
						</div>
					</ScrollArea.Viewport>
					<ScrollArea.Scrollbar orientation="vertical" className="z-10 flex w-2 touch-none select-none p-0.5">
						<ScrollArea.Thumb className="relative flex-1 rounded-full bg-muted-foreground/40" />
					</ScrollArea.Scrollbar>
				</main>
			</ScrollArea.Root>
			<DocumentReadingContext.Provider value={null}>
				<ArtifactPreviewModal
					artifact={previewRow?.row ?? null}
					projectName={preview?.projectName ?? ""}
					pinId={preview ? galleryArtifactKey(preview.row) : undefined}
					onClose={() => setPreview(null)}
				/>
			</DocumentReadingContext.Provider>
			<ConfirmDialog
				busy={bulkDelete.isPending}
				confirmLabel={t("artifacts.bulkDeleteConfirm", { count: selectedRows.length })}
				description={t("artifacts.bulkDeleteDescription")}
				destructive
				error={bulkDelete.error?.message ?? null}
				onConfirm={() => bulkDelete.mutate(selectedRows)}
				onOpenChange={setConfirmingBulkDelete}
				open={confirmingBulkDelete}
				title={t("artifacts.bulkDeleteTitle", { count: selectedRows.length })}
			>
				<ul aria-label={t("artifacts.selection")} className="space-y-0.5 text-control text-foreground">
					{selectedRows.slice(0, MAX_CONFIRM_NAMES).map((row) => <li key={galleryArtifactKey(row)} className="truncate">{row.name}</li>)}
					{selectedRows.length > MAX_CONFIRM_NAMES ? (
						<li className="text-muted-foreground">{t("artifacts.bulkDeleteMore", { count: selectedRows.length - MAX_CONFIRM_NAMES })}</li>
					) : null}
				</ul>
			</ConfirmDialog>
		</div>
	);
}

/** `values` with `value` added, or removed when it is already there. */
function toggled<T>(values: readonly T[], value: T): readonly T[] {
	return values.includes(value) ? values.filter((item) => item !== value) : [...values, value];
}

/**
 * One value of a filter. Choosing it keeps the menu open, so several values,
 * in one filter or several, are picked in one go.
 */
function FilterOption({ checked, onToggle, children }: { checked: boolean; onToggle: () => void; children: ReactNode }) {
	return (
		<DropdownMenuCheckboxItem checked={checked} onCheckedChange={onToggle} onSelect={(event) => event.preventDefault()}>
			{children}
		</DropdownMenuCheckboxItem>
	);
}

const ArtifactCard = memo(function ArtifactCard({ row, pinId, projectName, view, showProjectName = false, legacyPin, previewLive = true, selection, onPreview }: { row: ArtifactRow & { connection: GalleryRow["connection"] }; pinId?: string; projectName: string; view: ArtifactViewMode; showProjectName?: boolean; legacyPin?: string; previewLive?: boolean; selection?: ArtifactCardSelection; onPreview: (row: GalleryRow, projectName: string) => void }) {
	const { t } = useTranslation();
	const id = pinId ?? galleryArtifactKey(row);
	const pinned = useArtifactsViewStore((state) => state.pinnedArtifactIds.includes(id));
	const togglePinnedArtifact = useArtifactsViewStore((state) => state.togglePinnedArtifact);
	const share = useArtifactShareFile(row);
	const requestWorkspaceFileOpen = useUiStore((state) => state.requestWorkspaceFileOpen);
	const navigateToSession = useNavigateToSession();
	const KindIcon = artifactFileIcon(row);
	const selectionDescriptionId = useId();
	const context = artifactContext(t, row, projectName);

	const open = () => {
		onPreview(row, projectName);
	};
	const openInSession = () => {
		requestWorkspaceFileOpen(row.sessionId, row.path, undefined, "artifact");
		navigateToSession(row.projectId, row.sessionId);
	};

	if (view === "list") {
		const toggle = selection && !selection.disabledReason ? selection.onToggle : undefined;
		return (
			<li>
				{/* Name and context truncate; the Published mark, the time and the
				    actions never shrink, so the time stays visible in a narrow
				    panel. The actions keep their space while hidden, so hovering
				    a row moves nothing. Below @3xl the context drops under the
				    name; below @lg Pin and Open in session live in the ⋯ menu. */}
				<div className={cn(
					// The hovered row stacks the interactive layer on the hover surface:
					// the hover surface alone is only a shade off the row's own.
					"group/artifact relative flex min-h-12 items-center gap-3 rounded-md bg-card py-1.5 ps-4 pe-2 transition-none hover:bg-accent focus-within:bg-accent @max-3xl:min-h-14",
					"before:pointer-events-none before:absolute before:inset-0 before:rounded-md hover:before:bg-interactive-active focus-within:before:bg-interactive-active",
					selection?.selected && "ring-2 ring-primary",
				)}>
					{selection ? <span id={selectionDescriptionId} className="sr-only">{selection.description}</span> : null}
					{selection ? (
						<Checkbox
							aria-describedby={selectionDescriptionId}
							aria-label={selection.label}
							checked={selection.selected}
							className="relative z-[2] transition-none duration-0"
							disabled={Boolean(selection.disabledReason)}
							onCheckedChange={toggle}
							title={selection.disabledReason}
						/>
					) : null}
					<Pressable
						className={cn(
							"flex min-w-0 flex-1 items-center gap-3 self-stretch after:absolute after:inset-0 after:z-[1] after:rounded-md focus-visible:after:ring-2 focus-visible:after:ring-ring/60",
							selection?.disabledReason && "cursor-not-allowed",
						)}
						onClick={selection ? toggle : open}
						title={selection?.disabledReason ?? `${context} · ${row.path}`}
						{...(selection ? { tabIndex: -1, "aria-hidden": true } : {})}
					>
						<KindIcon aria-hidden="true" className="relative z-[1] size-icon-sm shrink-0 text-muted-foreground" />
						<span className="relative z-[1] flex min-w-0 flex-1 flex-col @3xl:flex-row @3xl:items-baseline @3xl:gap-2.5">
							<span className="flex min-w-0 items-center gap-1.5 @3xl:max-w-[60%] @3xl:shrink-0">
								{pinned ? (
									<span role="img" aria-label={t("artifacts.pinned")} className="inline-flex shrink-0 text-passive">
										<Pin aria-hidden="true" className="size-3" />
									</span>
								) : null}
								<span className="min-w-0 truncate text-control font-medium text-foreground">{row.name}</span>
							</span>
							<span className="min-w-0 truncate text-xs text-muted-foreground">{context}</span>
						</span>
					</Pressable>
					<div className="relative z-[2] flex shrink-0 items-center gap-3">
						{/* Empty for an unshared artifact, so every row's time lines up. */}
						<span className="inline-flex size-icon-sm items-center justify-center">
							<ArtifactShareMark row={row} iconClassName="size-icon-sm" />
						</span>
						{/* "14h ago", and "Updated 14h ago" while the row is hovered or focused.
						    A title, not a Tooltip: one per row costs too much to mount in a long list. */}
						<time dateTime={row.updatedAt} title={new Date(row.updatedAt).toLocaleString()} className="min-w-14 whitespace-nowrap text-right text-xs tabular-nums text-passive group-hover/artifact:text-foreground group-focus-within/artifact:text-foreground">
							<span className="@lg:group-hover/artifact:hidden @lg:group-focus-within/artifact:hidden">{formatTimeCompact(row.updatedAt)}</span>
							<span className="hidden @lg:group-hover/artifact:inline @lg:group-focus-within/artifact:inline">{t("shell.updatedAt", { time: formatTimeCompact(row.updatedAt) })}</span>
						</time>
					</div>
					{selection ? null : <div className="relative z-[2] flex shrink-0 items-center gap-1 @max-lg:[&>button]:hidden">
						{row.bundle ? null : <DeferredArtifactActionsMenu
							artifact={row}
						pinId={id}
							session={{ id: row.sessionId, cloud: row.cloud }}
							iconOnly
							share={share}
							onOpenInSession={row.kept ? undefined : openInSession}
							pinned={pinned}
							onTogglePin={() => togglePinnedArtifact(id, legacyPin)}
							className="bg-[var(--solid-bg-secondary,var(--color-bg-secondary))] hover:bg-[var(--solid-accent,var(--accent))] dark:hover:bg-[var(--solid-accent,var(--accent))] transition-none duration-0 [@media(hover:hover)]:opacity-0 group-hover/artifact:opacity-100 group-focus-within/artifact:opacity-100 data-[state=open]:opacity-100"
						/>}
					</div>}
				</div>
			</li>
		);
	}

	return (
		<ArtifactGalleryCard
			row={row}
						context={showProjectName ? `${projectName} · ${row.sessionTitle}` : row.sessionTitle}
			previewLive={previewLive}
			onPreview={open}
			selection={selection}
			actions={selection || row.bundle ? null : (
				<DeferredArtifactActionsMenu
					artifact={row}
							pinId={id}
					session={{ id: row.sessionId, cloud: row.cloud }}
					iconOnly
					share={share}
					onOpenInSession={row.kept ? undefined : openInSession}
					pinned={pinned}
					onTogglePin={() => togglePinnedArtifact(id, legacyPin)}
					className="bg-[var(--solid-bg-secondary,var(--color-bg-secondary))] hover:bg-[var(--solid-accent,var(--accent))] dark:hover:bg-[var(--solid-accent,var(--accent))] transition-none duration-0 [@media(hover:hover)]:opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 data-[state=open]:opacity-100"
				/>
			)}
		/>
	);
});

function EmptyArtifacts({ workspaces }: { workspaces: readonly WorkspaceSummary[] }) {
	const { t } = useTranslation();
	const openGlobalSettings = useUiStore((state) => state.openGlobalSettings);
	const { agentReady, create, pending } = useCreateArtifactSession();
	const hasCloudProject = workspaces.some((workspace) => workspace.kind === CLOUD_PROJECT_KIND && Boolean(workspace.cloudOrgId));

	// Without a local agent a cloud project, which runs its own, can still start one.
	if (!agentReady && !hasCloudProject) {
		return (
			<div className="flex flex-col items-center gap-1 py-24 text-center">
				<h2 className="text-heading-sm font-semibold text-foreground">{t("artifacts.noAgent.title")}</h2>
				<p className="max-w-72 text-xs text-muted-foreground">{t("artifacts.noAgent.description")}</p>
				<Button variant="ghost" size="sm" className="mt-2" onClick={() => openGlobalSettings("harness")}>
					{t("artifacts.noAgent.action")}
				</Button>
			</div>
		);
	}
	return (
		<div className="flex flex-col items-center gap-1 py-24 text-center">
			<h2 className="text-heading-sm font-semibold text-foreground">{t("artifacts.empty.title")}</h2>
			<p className="max-w-72 text-xs text-muted-foreground">{t("artifacts.empty.description")}</p>
			<CreateArtifactMenu
				workspaces={workspaces}
				create={create}
				pending={pending}
				className="mt-3"
				align="center"
				localDisabledReason={agentReady ? undefined : t("artifacts.noAgent.description")}
			/>
		</div>
	);
}

/**
 * The Create artifact trigger and its Scratchpad / project menu. The empty
 * state and the list header share it so both start the same session.
 */
function CreateArtifactMenu({
	workspaces,
	create,
	pending,
	label = "create",
	align,
	className,
	localDisabledReason,
}: {
	workspaces: readonly WorkspaceSummary[];
	create: (projectId?: string, cloudOrgId?: string) => Promise<void>;
	pending: boolean;
	label?: "create" | "new";
	align: "center" | "end";
	className?: string;
	/** Why Scratchpad and local projects cannot start a session; cloud projects run their own agent. */
	localDisabledReason?: string;
}) {
	const { t } = useTranslation();
	const projectAvatars = useProjectAvatars(workspaces);
	// Cloud workspaces are listed only when the control plane keeps artifacts.
	const projects = workspaces.filter(
		(workspace) =>
			workspace.id !== STANDALONE_WORKSPACE_ID &&
			!workspace.hostId &&
			(workspace.kind !== CLOUD_PROJECT_KIND || Boolean(workspace.cloudOrgId)),
	);
	const localBlocked = Boolean(localDisabledReason);
	const hasCloudProject = projects.some((project) => project.kind === CLOUD_PROJECT_KIND);
	const disabledReason = localBlocked && !hasCloudProject ? localDisabledReason : undefined;
	const disabled = pending || Boolean(disabledReason);
	const trigger =
		label === "new" ? (
			<Button type="button" size="icon-sm"
				variant="quiet"
				className={className}
				disabled={disabled}
				aria-busy={pending}
				aria-label={pending ? t("artifacts.creating") : t("artifacts.new")}
			>
				{pending ? (
					<Loader2 aria-hidden="true" className="size-icon-sm animate-spin motion-reduce:animate-none" />
				) : (
					<Plus aria-hidden="true" className="size-icon-sm" />
				)}
			</Button>
		) : (
			<Button size="sm" className={className} disabled={disabled} aria-busy={pending}>
				{pending ? <Loader2 aria-hidden="true" className="size-icon-sm animate-spin text-status-working motion-reduce:animate-none" /> : null}
				{pending ? t("artifacts.creating") : t("artifacts.create")}
				{pending ? null : <ChevronDown aria-hidden="true" className="size-icon-sm" />}
			</Button>
		);
	if (disabledReason) {
		return (
			<Tooltip>
				<TooltipTrigger asChild>
					<span tabIndex={0} className="inline-flex rounded-md focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60">
						{trigger}
					</span>
				</TooltipTrigger>
				<TooltipContent>{disabledReason}</TooltipContent>
			</Tooltip>
		);
	}
	return (
		<DropdownMenu>
			{label === "new" ? (
				<Tooltip>
					<TooltipTrigger asChild>
						<DropdownMenuTrigger asChild>{trigger}</DropdownMenuTrigger>
					</TooltipTrigger>
					<TooltipContent side="bottom">{t("artifacts.new")}</TooltipContent>
				</Tooltip>
			) : (
				<DropdownMenuTrigger asChild>{trigger}</DropdownMenuTrigger>
			)}
			<DropdownMenuContent align={align} className="min-w-56">
				{/* Says why Scratchpad and local projects are disabled while cloud ones are not. */}
				{localBlocked ? <DropdownMenuLabel className="max-w-64 text-xs font-normal text-muted-foreground">{localDisabledReason}</DropdownMenuLabel> : null}
				<DropdownMenuItem disabled={localBlocked} onSelect={() => void create()}>
					<File aria-hidden="true" className="size-icon-sm" />
					{t("standalone.workspaceName")}
				</DropdownMenuItem>
				{projects.length > 0 ? <DropdownMenuSeparator /> : null}
				{projects.map((project) => (
					<DropdownMenuItem
						key={project.id}
						disabled={localBlocked && project.kind !== CLOUD_PROJECT_KIND}
						onSelect={() => void create(project.id, project.kind === CLOUD_PROJECT_KIND ? project.cloudOrgId : undefined)}
					>
						<ArtifactProjectIcon avatarUrl={projectAvatars.get(project.id)} workspace={project} />
						<span className="truncate">{project.name}</span>
					</DropdownMenuItem>
				))}
			</DropdownMenuContent>
		</DropdownMenu>
	);
}
