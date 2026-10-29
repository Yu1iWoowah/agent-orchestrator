import { Copy, File, FileText, MoreHorizontal, Pin } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { ArtifactActionsMenu } from "./ArtifactActionsMenu";
import { useTranslation } from "react-i18next";
import type { CatalogArtifactRow } from "../lib/gallery-artifacts";
import { galleryArtifactKey } from "../lib/gallery-artifacts";
import { copyDocument } from "../lib/document-clipboard";
import { formatTimeCompact } from "../lib/format-time";
import { useNavigateToSession } from "../lib/navigate-to-session";
import { useArtifactsViewStore, type ArtifactViewMode } from "../stores/artifacts-view-store";
import { useUiStore } from "../stores/ui-store";
import { ArtifactGalleryCard } from "./ArtifactGalleryCard";
import { Button } from "./ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "./ui/dropdown-menu";

export function CatalogArtifactCard({
	row,
	projectName,
	view,
	onOpen,
	legacyPin,
}: {
	row: CatalogArtifactRow;
	projectName: string;
	view: ArtifactViewMode;
	onOpen: () => void;
	legacyPin?: string;
}) {
	const { t } = useTranslation();
	const navigate = useNavigateToSession();
	const client = useQueryClient();
	const id = galleryArtifactKey(row);
	const pinned = useArtifactsViewStore((state) => state.pinnedArtifactIds.includes(id));
	const togglePin = () => useArtifactsViewStore.getState().togglePinnedArtifact(id, legacyPin);
	const names = {
		worker: t("artifacts.source.worker"),
		orchestrator: t("artifacts.source.orchestrator"),
		shared: t("artifacts.source.shared"),
	};
	const context = [
		projectName,
		names[row.source],
		row.sessionTitle,
		row.archived ? t("artifacts.archive.archive") : undefined,
	]
		.filter(Boolean)
		.join(" · ");
	const copy = async (kind: "path" | "link") => {
		try {
			const resolved = await copyDocument(row.connection, row.target, kind);
			useUiStore
				.getState()
				.showGlobalToast(t(resolved ? "documents.sheet.copied" : "documents.sheet.copiedUnresolved"));
		} catch {
			useUiStore.getState().showGlobalToast(t("documents.sheet.copyError"), undefined, { tone: "error" });
		}
	};
	const copyActions = (
		<>
			<DropdownMenuItem onSelect={() => void copy("link")}>
				<Copy className="size-4" />
				{t("documents.sheet.copyLink")}
			</DropdownMenuItem>
			<DropdownMenuItem onSelect={() => void copy("path")}>
				<Copy className="size-4" />
				{t("documents.sheet.copyPath")}
			</DropdownMenuItem>
		</>
	);
	const target = row.target;
	const actions =
		target.kind === "artifact" && row.connection.kind === "local" ? (
			<ArtifactActionsMenu
				artifact={{
					name: row.name,
					path: target.path,
					updatedAt: row.updatedAt ?? undefined,
					rawUrl: row.connection.identity
						? `${row.connection.identity}/api/v1/sessions/${encodeURIComponent(target.sessionId)}/artifact-files/${target.path.split("/").map(encodeURIComponent).join("/")}?source=1`
						: undefined,
				}}
				session={{ id: target.sessionId }}
				pinId={id}
				iconOnly
				pinInMenu
				pinned={pinned}
				onTogglePin={togglePin}
				onOpenInSession={() => {
					useUiStore.getState().requestWorkspaceFileOpen(target.sessionId, target.path, undefined, "artifact");
					navigate(target.projectId, target.sessionId);
				}}
				onDeleted={() =>
					client.invalidateQueries({ queryKey: ["artifact-catalogs", row.connection.kind, row.connection.identity] })
				}
			>
				{copyActions}
			</ArtifactActionsMenu>
		) : (
			<DropdownMenu>
				<DropdownMenuTrigger asChild>
					<Button variant="ghost" size="icon-sm" aria-label={`${t("files.moreFileActions")}: ${row.name}`}>
						<MoreHorizontal className="size-4" />
					</Button>
				</DropdownMenuTrigger>
				<DropdownMenuContent align="end">
					<DropdownMenuItem onSelect={togglePin}>
						<Pin className="size-4" />
						{t(pinned ? "artifacts.unpin" : "artifacts.pin")}
					</DropdownMenuItem>
					{copyActions}
				</DropdownMenuContent>
			</DropdownMenu>
		);
	const kind = /\.html?$/i.test(row.path) ? "html" : /\.(md|markdown)$/i.test(row.path) ? "markdown" : "file";
	const Icon = kind === "markdown" ? FileText : File;
	if (view === "gallery")
		return (
			<ArtifactGalleryCard
				row={{ name: row.name, path: row.path, kind, updatedAt: row.updatedAt }}
				context={context}
				detail={row.path}
				actions={actions}
				onPreview={onOpen}
			/>
		);
	return (
		<li className="group/artifact flex min-h-11 min-w-0 items-center gap-2 rounded-md bg-card px-3 hover:bg-accent focus-within:bg-accent">
			<button
				type="button"
				onClick={onOpen}
				className="min-w-0 flex-1 py-2 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
				title={row.path}
			>
				<span className="flex items-center gap-2 text-control font-medium">
					<Icon aria-hidden="true" className="size-4 shrink-0" />
					{row.name}
				</span>
				<span className="block text-xs text-muted-foreground">{context}</span>
				<span className="block break-all font-mono text-xs text-muted-foreground">{row.path}</span>
			</button>
			{actions}
			{row.updatedAt ? (
				<time className="shrink-0 text-xs tabular-nums text-muted-foreground" dateTime={row.updatedAt}>
					{formatTimeCompact(row.updatedAt)}
				</time>
			) : (
				<span className="max-w-28 text-xs text-muted-foreground">{t("artifacts.unknownUpdated")}</span>
			)}
		</li>
	);
}
