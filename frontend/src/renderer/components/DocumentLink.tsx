import { useState, type ComponentProps } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Copy, FileText, ArrowUpRight } from "lucide-react";
import {
	resolveDocument,
	documentTargetKey,
	type DocumentInput,
} from "../lib/document-read";
import {
	openReadingDocument,
	useDocumentReader,
} from "../lib/document-reading-context";
import { copyDocument } from "../lib/document-clipboard";
import { useUiStore } from "../stores/ui-store";
import { useNavigateToSession } from "../lib/navigate-to-session";
import {
	ContextMenu,
	ContextMenuTrigger,
	ContextMenuContent,
	ContextMenuItem,
} from "./ui/context-menu";
import { Tooltip, TooltipTrigger, TooltipContent } from "./ui/tooltip";
export function DocumentLink({
	input,
	...props
}: ComponentProps<"a"> & { input: DocumentInput }) {
	const reader = useDocumentReader()!;
	const { t } = useTranslation();
	const [hover, setHover] = useState(false);
	const [menu, setMenu] = useState(false);
	const query = useQuery({
		queryKey: documentTargetKey(reader.connection, input),
		queryFn: ({ signal }) => resolveDocument(reader.connection, input, signal),
		enabled: hover || menu,
		staleTime: 5000,
	});
	const copy = async (kind: "path" | "link") => {
		try {
			await copyDocument(reader.connection, input, kind);
			useUiStore.getState().showGlobalToast(t("documents.sheet.copied"));
		} catch {
			useUiStore
				.getState()
				.showGlobalToast(t("documents.sheet.copyError"), undefined, {
					tone: "error",
				});
		}
	};
	return (
		<Tooltip open={hover && !menu}>
			<ContextMenu onOpenChange={setMenu}>
				<ContextMenuTrigger asChild>
					<TooltipTrigger asChild>
						<a
							{...props}
							onMouseEnter={() => setHover(true)}
							onMouseLeave={() => setHover(false)}
							onFocus={() => setHover(true)}
							onBlur={() => setHover(false)}
							onClick={(event) => {
								event.preventDefault();
								openReadingDocument(reader, input);
							}}
						/>
					</TooltipTrigger>
				</ContextMenuTrigger>
				<ContextMenuContent>
					<ContextMenuItem onSelect={() => openReadingDocument(reader, input)}>
						<FileText />
						{t("documents.sheet.open")}
					</ContextMenuItem>
					<ContextMenuItem onSelect={() => void copy("link")}>
						<Copy />
						{t("documents.sheet.copyLink")}
					</ContextMenuItem>
					<ContextMenuItem onSelect={() => void copy("path")}>
						<Copy />
						{t("documents.sheet.copyPath")}
					</ContextMenuItem>
					{query.data?.target.kind === "artifact" ? (
						<DocumentSourceMenu
							projectId={query.data.target.projectId}
							sessionId={query.data.target.sessionId}
						/>
					) : null}
				</ContextMenuContent>
			</ContextMenu>
			<TooltipContent
				className="pointer-events-none max-w-md break-all"
				aria-live="polite"
			>
				{query.data ? (
					<>
						<div>{query.data.localPath}</div>
						<div>
							{t("documents.sheet.metadata", {
								size: query.data.size,
								media: query.data.mediaType,
							})}
						</div>
					</>
				) : query.isError ? (
					query.error &&
					"code" in query.error &&
					query.error.code === "DOCUMENT_SOURCE_MISSING" ? (
						t("documents.sheet.notCreated")
					) : (
						t("documents.sheet.unavailable")
					)
				) : (
					t("documents.sheet.loading")
				)}
			</TooltipContent>
		</Tooltip>
	);
}

function DocumentSourceMenu({
	projectId,
	sessionId,
}: {
	projectId: string;
	sessionId: string;
}) {
	const navigate = useNavigateToSession();
	const { t } = useTranslation();
	return (
		<ContextMenuItem onSelect={() => navigate(projectId, sessionId)}>
			<ArrowUpRight />
			{t("documents.sheet.goSource")}
		</ContextMenuItem>
	);
}
