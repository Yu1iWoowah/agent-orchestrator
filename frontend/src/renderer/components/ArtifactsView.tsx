import { BrowserIcon } from "./icons";
import {
	ChevronDown,
	Clock,
	Cloud,
	File,
	FileText,
	Folder,
	LayoutGrid,
	List,
	Loader2,
	Pin,
	Plus,
	Search,
	RefreshCw,
} from "lucide-react";
import { ScrollArea } from "radix-ui";
import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useShallow } from "zustand/react/shallow";
import { DocumentReadError } from "../lib/document-read";
import { useCloudOffering } from "../hooks/useCloudOffering";
import { useCreateArtifactSession } from "../hooks/useCreateArtifactSession";
import { useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import type { ArtifactRow } from "../lib/artifacts";
import {
	catalogArtifactGroup,
	filterGalleryGroups,
	galleryArtifactKey,
	galleryLegacyKey,
	isCatalogArtifact,
	legacyGalleryGroups,
	newestGalleryFirst,
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
import { formatTimeCompact } from "../lib/format-time";
import { useNavigateToSession } from "../lib/navigate-to-session";
import { hidesShellTopbar } from "../lib/platform";
import { cn } from "../lib/utils";
import {
	defaultGalleryPreferences,
	useArtifactsViewStore,
	type ArtifactViewMode,
	type GalleryPreferences,
} from "../stores/artifacts-view-store";
import { useUiStore } from "../stores/ui-store";
import { CLOUD_PROJECT_KIND, STANDALONE_WORKSPACE_ID, type WorkspaceSummary } from "../types/workspace";
import { ArtifactActionsMenu } from "./ArtifactActionsMenu";
import { ArtifactGalleryCard } from "./ArtifactGalleryCard";
import { ArtifactPreviewModal } from "./ArtifactPreviewModal";
import { TopbarButton, topbarProjectLabelClass } from "./TopbarButton";
import { Button } from "./ui/button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuLabel,
	DropdownMenuRadioGroup,
	DropdownMenuRadioItem,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "./ui/dropdown-menu";
import { Input } from "./ui/input";
import { Tooltip, TooltipContent, TooltipTrigger } from "./ui/tooltip";

const ARTIFACT_GRID_CLASS = "grid grid-cols-[repeat(auto-fill,minmax(min(100%,18rem),1fr))] gap-4";

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
	const catalog = useArtifactCatalogs(workspaces);
	const { query, projectId, source, archive, visibleCount } = useArtifactsViewStore(
		useShallow((state) => {
			const { query, projectId, source, archive, visibleCount } =
				state.galleryPreferences[catalog.contextKey] ?? defaultGalleryPreferences;
			return { query, projectId, source, archive, visibleCount };
		}),
	);
	const setPreferences = useArtifactsViewStore((state) => state.setGalleryPreferences);
	const view = useArtifactsViewStore((state) => state.view),
		setView = useArtifactsViewStore((state) => state.setView);
	const pins = useArtifactsViewStore((state) => state.pinnedArtifactIds);
	const [preview, setPreview] = useState<{
		row: ArtifactRow & { connection: GalleryRow["connection"] };
		projectName: string;
	} | null>(null);
	const groups = useMemo(
		() =>
			[
				...catalog.projects.map((project) =>
					catalogArtifactGroup(
						project,
						catalog.connection,
						catalog.data?.find((entry) => entry.projectId === project.id)?.items ?? [],
					),
				),
				...legacyGalleryGroups(workspaces ?? [], catalog.connection),
			].sort((a, b) =>
				a.rows[0] && b.rows[0]
					? newestGalleryFirst(a.rows[0], b.rows[0])
					: a.rows.length
						? -1
						: b.rows.length
							? 1
							: a.id.localeCompare(b.id),
			),
		[catalog.projects, catalog.connection, catalog.data, workspaces],
	);
	const allRows = useMemo(() => groups.flatMap((group) => group.rows), [groups]);
	const complete = catalog.data !== undefined && catalog.data.every((entry) => !entry.loading);
	const incomplete = !complete || Boolean(catalog.data?.some((entry) => entry.incomplete || entry.error));
	const migrateSafe =
		complete &&
		!catalog.data?.some(
			(entry) =>
				entry.error ||
				(entry.incomplete &&
					(entry.errors.length === 0 ||
						entry.errors.some((error) => error.source !== "shared" || error.code !== "DOCUMENT_SOURCE_MISSING"))),
		);
	useEffect(() => {
		if (migrateSafe) useArtifactsViewStore.getState().migratePins(allRows);
	}, [migrateSafe, allRows]);
	const legacyPins = useMemo(() => {
		const mapping = new Map<string, string | null>();
		if (migrateSafe)
			for (const row of allRows) {
				const legacy = galleryLegacyKey(row);
				if (legacy) mapping.set(legacy, mapping.has(legacy) ? null : galleryArtifactKey(row));
			}
		return mapping;
	}, [allRows, migrateSafe]);
	const scratchpadName = t("standalone.workspaceName");
	const visibleGroups = useMemo(
		() => filterGalleryGroups(groups, { query, projectId, source, archive }, scratchpadName),
		[groups, query, projectId, source, archive, scratchpadName],
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
	const shown = new Set(cards.slice(0, visibleCount).map(({ row }) => galleryArtifactKey(row)));
	const showProjectSections = visibleGroups.length > 1;
	const openPreview = (row: GalleryRow, projectName: string) => {
		if (isCatalogArtifact(row)) {
			if (reader) openReadingDocument(reader, row.target);
		} else setPreview({ row, projectName });
	};
	const card = (row: GalleryRow, projectName: string) => {
		const key = galleryArtifactKey(row),
			legacy = galleryLegacyKey(row);
		const alias = legacy && legacyPins.get(legacy) === key ? legacy : undefined;
		return isCatalogArtifact(row) ? (
			<CatalogArtifactCard
				key={key}
				row={row}
				projectName={projectName}
				view={view}
				legacyPin={alias}
				onOpen={() => openPreview(row, projectName)}
			/>
		) : (
			<ArtifactCard
				key={key}
				row={row}
				projectName={projectName}
				view={view}
				showProjectName={!showProjectSections}
				legacyPin={alias}
				onPreview={openPreview}
			/>
		);
	};
	const changeFilters = (change: Partial<GalleryPreferences>) => {
		restored.current = catalog.contextKey;
		setPreferences(catalog.contextKey, { ...change, scrollTop: 0, visibleCount: 200 });
		if (viewport.current) viewport.current.scrollTop = 0;
	};
	const clearFilters = () => changeFilters({ query: "", projectId: "", source: "all", archive: "all" });
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
					: entry.errors),
			].map((error) => ({ ...error, projectId: entry.projectId })),
		) ?? [];
	return (
		<div ref={surface} className="relative flex min-h-0 flex-1 flex-col bg-background">
			<header className="workspace-topbar-container center-panel-titlebar flex h-toolbar shrink-0 items-center justify-between gap-3 border-b border-border-strong pr-[calc((var(--size-toolbar)-var(--size-topbar-control))/2)]">
				<h1 className={showPageTitle ? cn(topbarProjectLabelClass, "inline-flex items-center gap-1.5") : "sr-only"}>
					{showPageTitle ? <File className="size-icon-md" aria-hidden="true" /> : null}
					{t("artifacts.title")}
				</h1>
				<div className="workspace-topbar-actions flex min-w-0 flex-1 items-center justify-end gap-2">
					<label className="relative block w-56 min-w-0 shrink">
						<Search
							aria-hidden="true"
							className="pointer-events-none absolute start-2.5 top-1/2 size-icon-sm -translate-y-1/2 text-passive"
						/>
						<Input
							aria-label={t("artifacts.search")}
							className="inspector-field-input ps-8"
							type="search"
							value={query}
							placeholder={t("artifacts.search")}
							onChange={(event) => changeFilters({ query: event.target.value })}
						/>
					</label>
					<DropdownMenu>
						<DropdownMenuTrigger asChild>
							<TopbarButton variant="secondary" aria-label={t("artifacts.projectFilter")} className="max-w-44">
								<Folder aria-hidden="true" className="size-icon-sm" />
								<span className="truncate">
									{groups.find((group) => group.id === projectId)?.isScratchpad ? scratchpadName : groups.find((group) => group.id === projectId)?.name ?? t("artifacts.allProjects")}
								</span>
								<ChevronDown className="size-icon-xs" />
							</TopbarButton>
						</DropdownMenuTrigger>
						<DropdownMenuContent align="end">
							<DropdownMenuRadioGroup value={projectId} onValueChange={(value) => changeFilters({ projectId: value })}>
								<DropdownMenuRadioItem value="">{t("artifacts.allProjects")}</DropdownMenuRadioItem>
								<DropdownMenuSeparator />
								{groups.map((group) => (
									<DropdownMenuRadioItem key={group.id} value={group.id}>
										{group.isScratchpad ? scratchpadName : group.name}
									</DropdownMenuRadioItem>
								))}
							</DropdownMenuRadioGroup>
						</DropdownMenuContent>
					</DropdownMenu>
					<div role="group" aria-label={t("artifacts.view")} className="inline-flex shrink-0 items-center">
						{(
							[
								{ value: "gallery", label: t("artifacts.galleryView"), Icon: LayoutGrid },
								{ value: "list", label: t("artifacts.listView"), Icon: List },
							] as const
						).map(({ value, label, Icon }) => (
							<Tooltip key={value}>
								<TooltipTrigger asChild>
									<TopbarButton
										variant="icon"
										aria-label={label}
										aria-pressed={view === value}
										onClick={() => setView(value)}
										className={cn(view === value && "bg-interactive-active")}
									>
										<Icon className="size-icon-sm" />
									</TopbarButton>
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
					/>
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
					{incomplete && allRows.length === 0
						? t("artifacts.totalUnknown")
						: t("artifacts.filesLoaded", { count: allRows.length })}
				</span>
				<DocumentSheet surfaceRef={surface} />
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
							if (restored.current === catalog.contextKey)
								setPreferences(catalog.contextKey, { scrollTop: event.currentTarget.scrollTop });
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
							{sourceProblems.length > 0 ? (
								<div className="py-2 text-sm">
									<p>{t("artifacts.sourcesIncomplete")}</p>
									<ul>
										{sourceProblems.map((error, index) => (
											<li
												key={`${error.projectId}:${error.source}:${error.sessionId}:${index}`}
												role={error.code === "DOCUMENT_SOURCE_MISSING" ? "status" : "alert"}
											>
												{catalog.projects.find((project) => project.id === error.projectId)?.name ?? error.projectId} ·{" "}
												{error.source === "worker" || error.source === "orchestrator" || error.source === "shared"
													? sourceNames[error.source]
													: sourceNames.all}{" "}
												{error.sessionId ?? ""}:{" "}
												{error.code === "DOCUMENT_SOURCE_MISSING"
													? t("documents.sheet.notCreated")
													: error.code === "DOCUMENT_CATALOG_CHANGED"
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
								<>
									{pinnedCards.length > 0 ? (
										<ArtifactSection sectionId="pinned" name={t("artifacts.pinned")} icon={Pin} view={view}>
											{pinnedCards.map(({ row, projectName }) => card(row, projectName))}
										</ArtifactSection>
									) : null}
									{showProjectSections ? (
										<ArtifactSection sectionId="recent" name={t("artifacts.recent")} icon={Clock} view={view}>
											{cards.slice(0, 3).map(({ row, projectName }) => card(row, projectName))}
										</ArtifactSection>
									) : null}
									{visibleGroups.map((group) => {
										const rows = group.rows.filter((row) => shown.has(galleryArtifactKey(row)));
										return rows.length ? (
											<ArtifactSection
												key={group.id}
												sectionId={`project:${group.id}`}
												name={group.isScratchpad ? scratchpadName : group.name}
												icon={group.isScratchpad ? File : group.cloud ? Cloud : Folder}
												view={view}
												collapsible={showProjectSections}
												updatedAt={group.rows[0]?.updatedAt ?? undefined}
											>
												{rows.map((row) => card(row, group.isScratchpad ? scratchpadName : group.name))}
											</ArtifactSection>
										) : null;
									})}
									{cards.length > visibleCount ? (
										<Button
											variant="ghost"
											onClick={() => setPreferences(catalog.contextKey, { visibleCount: visibleCount + 200 })}
										>
											{t("artifacts.loadMore")}
										</Button>
									) : null}
								</>
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
					artifact={preview?.row ?? null}
					projectName={preview?.projectName ?? ""}
					pinId={preview ? galleryArtifactKey(preview.row) : undefined}
					onClose={() => setPreview(null)}
				/>
			</DocumentReadingContext.Provider>
		</div>
	);
}

function ArtifactSection({ sectionId, name, icon: Icon, updatedAt, children, view, collapsible = true }: {
	sectionId: string;
	name: string;
	icon: typeof Folder;
	updatedAt?: string;
	children: ReactNode;
	view: ArtifactViewMode;
	collapsible?: boolean;
}) {
	const { t } = useTranslation();
	const expanded = useArtifactsViewStore((state) => !collapsible || !state.collapsedSections.includes(sectionId));
	const toggleSection = useArtifactsViewStore((state) => state.toggleSection);
	const gridId = useId();
	const headerContent = <>
		<span className={cn(topbarProjectLabelClass, "inline-flex min-w-0 items-center gap-1.5")}>
			<Icon aria-hidden="true" className="size-icon-md shrink-0" />
			<span className="truncate">{name}</span>
		</span>
		{updatedAt ? (
			<time dateTime={updatedAt} title={new Date(updatedAt).toLocaleString()} className="shrink-0 text-caption font-normal tabular-nums text-muted-foreground">
				{t("shell.updatedAt", { time: formatTimeCompact(updatedAt) })}
			</time>
		) : null}
	</>;
	return (
		<section aria-label={name} className={expanded ? "mb-3" : undefined}>
			<header>
				<h2 aria-label={name}>
					{collapsible ? <button
						type="button"
						aria-label={name}
						aria-expanded={expanded}
						aria-controls={gridId}
						onClick={() => toggleSection(sectionId)}
						className="flex min-h-11 w-full min-w-0 cursor-pointer items-center gap-3 rounded-md text-left transition-none focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
					>
						{headerContent}
						<ChevronDown aria-hidden="true" className={cn("size-icon-sm shrink-0 text-muted-foreground", !expanded && "-rotate-90")} />
					</button> : <div className="flex min-h-11 min-w-0 items-center gap-3">{headerContent}</div>}
				</h2>
			</header>
			<ul id={gridId} aria-label={name} hidden={!expanded} className={expanded ? (view === "gallery" ? ARTIFACT_GRID_CLASS : "grid gap-2") : "hidden"}>
				{children}
			</ul>
		</section>
	);
}

function ArtifactCard({ row, projectName, view, showProjectName = false, legacyPin, onPreview }: { row: ArtifactRow & { connection: GalleryRow["connection"] }; projectName: string; view: ArtifactViewMode; showProjectName?: boolean; legacyPin?: string; onPreview: (row: GalleryRow, projectName: string) => void }) {
	const { t } = useTranslation();
	const id = galleryArtifactKey(row);
	const pinned = useArtifactsViewStore((state) => state.pinnedArtifactIds.includes(id));
	const togglePinnedArtifact = useArtifactsViewStore((state) => state.togglePinnedArtifact);
	const requestWorkspaceFileOpen = useUiStore((state) => state.requestWorkspaceFileOpen);
	const navigateToSession = useNavigateToSession();
	const KindIcon = row.kind === "html" ? BrowserIcon : row.kind === "markdown" ? FileText : File;

	const open = () => {
		onPreview(row, projectName);
	};
	const openInSession = () => {
		requestWorkspaceFileOpen(row.sessionId, row.path, undefined, "artifact");
		navigateToSession(row.projectId, row.sessionId);
	};

	if (view === "list") {
		return (
			<li>
				<div className="group/artifact relative flex min-h-11 items-center gap-2.5 rounded-md bg-card px-3 transition-none hover:bg-accent focus-within:bg-accent">
					<button
						type="button"
						className="flex min-w-0 flex-1 items-center gap-2.5 self-stretch text-left outline-none after:absolute after:inset-0 after:z-[1] after:rounded-md focus-visible:after:ring-2 focus-visible:after:ring-ring/60"
						onClick={open}
						title={`${row.sessionTitle} · ${row.path}`}
					>
						{row.generating ? (
							<Loader2 aria-label={t("artifacts.generating")} className="relative z-[1] size-icon-sm shrink-0 animate-spin text-status-working motion-reduce:animate-none" />
						) : <KindIcon aria-hidden="true" className="relative z-[1] size-icon-sm shrink-0 text-muted-foreground" />}
						<span className="relative z-[1] min-w-0 shrink py-2 text-control font-medium text-foreground"><span className="block truncate">{row.name}</span><span className="block break-all font-mono text-xs font-normal text-muted-foreground">{row.path}</span></span>
						<span className="relative z-[1] min-w-0 flex-1 truncate text-xs text-muted-foreground">
							{showProjectName ? `${projectName} · ${row.sessionTitle}` : row.sessionTitle}
						</span>
					</button>
					<div className="relative z-[2] ms-auto flex shrink-0 items-center gap-1">
						<ArtifactActionsMenu
							artifact={row}
						pinId={id}
							session={{ id: row.sessionId, cloud: row.cloud }}
							iconOnly
							onOpenInSession={openInSession}
							pinned={pinned}
							onTogglePin={() => togglePinnedArtifact(id, legacyPin)}
							className="bg-[var(--solid-bg-secondary,var(--color-bg-secondary))] hover:bg-[var(--solid-accent,var(--accent))] dark:hover:bg-[var(--solid-accent,var(--accent))] transition-none duration-0 [@media(hover:hover)]:opacity-0 group-hover/artifact:opacity-100 group-focus-within/artifact:opacity-100 data-[state=open]:opacity-100"
						/>
					</div>
					<time dateTime={row.updatedAt} className="pointer-events-none relative z-[1] w-20 shrink-0 text-right text-xs tabular-nums text-passive">
						{formatTimeCompact(row.updatedAt)}
					</time>
				</div>
			</li>
		);
	}

	return (
		<ArtifactGalleryCard
			row={row}
			detail={row.path}
			context={showProjectName ? `${projectName} · ${row.sessionTitle}` : row.sessionTitle}
			onPreview={open}
			actions={
				<ArtifactActionsMenu
					artifact={row}
							pinId={id}
					session={{ id: row.sessionId, cloud: row.cloud }}
					iconOnly
					onOpenInSession={openInSession}
					pinned={pinned}
					onTogglePin={() => togglePinnedArtifact(id, legacyPin)}
					className="bg-[var(--solid-bg-secondary,var(--color-bg-secondary))] hover:bg-[var(--solid-accent,var(--accent))] dark:hover:bg-[var(--solid-accent,var(--accent))] transition-none duration-0 [@media(hover:hover)]:opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 data-[state=open]:opacity-100"
				/>
			}
		/>
	);
}

function EmptyArtifacts({ workspaces }: { workspaces: readonly WorkspaceSummary[] }) {
	const { t } = useTranslation();
	const openGlobalSettings = useUiStore((state) => state.openGlobalSettings);
	// Developer mode moves account management to the Harness page.
	const agentsSection = useUiStore((state) => state.developerMode) ? "harness" : "agents";
	const { agentReady, create, pending } = useCreateArtifactSession();
	const hasCloudProject = workspaces.some((workspace) => workspace.kind === CLOUD_PROJECT_KIND && Boolean(workspace.cloudOrgId));

	// Without a local agent a cloud project, which runs its own, can still start one.
	if (!agentReady && !hasCloudProject) {
		return (
			<div className="flex flex-col items-center gap-1 py-24 text-center">
				<h2 className="text-heading-sm font-semibold text-foreground">{t("artifacts.noAgent.title")}</h2>
				<p className="max-w-72 text-xs text-muted-foreground">{t("artifacts.noAgent.description")}</p>
				<Button variant="ghost" size="sm" className="mt-2" onClick={() => openGlobalSettings(agentsSection)}>
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
			// Anchors the menu, so it must not scale on press.
			<TopbarButton
				variant="primary"
				className={cn("topbar-control--labeled topbar-control--static-press", className)}
				disabled={disabled}
				aria-busy={pending}
			>
				{pending ? (
					<Loader2 aria-hidden="true" className="size-icon-md animate-spin motion-reduce:animate-none" />
				) : (
					<Plus aria-hidden="true" className="size-icon-md" />
				)}
				{pending ? t("artifacts.creating") : t("artifacts.new")}
			</TopbarButton>
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
			<DropdownMenuTrigger asChild>{trigger}</DropdownMenuTrigger>
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
						{project.kind === CLOUD_PROJECT_KIND ? (
							<Cloud aria-hidden="true" className="size-icon-sm" />
						) : (
							<Folder aria-hidden="true" className="size-icon-sm" />
						)}
						<span className="truncate">{project.name}</span>
					</DropdownMenuItem>
				))}
			</DropdownMenuContent>
		</DropdownMenu>
	);
}
