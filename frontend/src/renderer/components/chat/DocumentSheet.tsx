import { downloadDocument } from "../../lib/document-download";
import { aoBridge } from "../../lib/bridge";
import { useEffect, useId, useRef, useState, type RefObject } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import {
	ArrowLeft,
	ArrowRight,
	Copy,
	RefreshCw,
	ZoomIn,
	ZoomOut,
	ArrowUpRight,
	Download,
} from "lucide-react";
import { useDocumentReader } from "../../lib/document-reading-context";
import {
	resolveDocument,
	DocumentReadError,
	type DocumentInput,
} from "../../lib/document-read";
import { documentFileKey } from "../../lib/document-targets";
import { copyDocument } from "../../lib/document-clipboard";
import { useNavigateToSession } from "../../lib/navigate-to-session";
import { useDocumentReadingStore } from "../../stores/document-reading-store";
import { useUiStore } from "../../stores/ui-store";
import {
	useConversationFrameStore,
	conversationFrameSessionKey,
} from "../../stores/conversation-frame-store";
import { useConversationFrameSafeRect } from "../../hooks/useConversationFrameSafeRect";
import { ResizableConversationFrame } from "./ConversationFrames";
import { Sheet, SheetContent, SheetTitle } from "../ui/sheet";
import { Button } from "../ui/button";
import { DocumentBody, documentErrorKey } from "../DocumentBody";
const limits = {
	width: 520,
	height: 600,
	minWidth: 280,
	minHeight: 160,
	maxWidth: 720,
	maxHeight: 900,
	entryWidth: 128,
};
const defaultSize = { width: 520, height: 600 };
export function DocumentSheet(props: {
	surfaceRef: RefObject<HTMLElement | null>;
	composerRef: RefObject<HTMLElement | null>;
}) {
	const reader = useDocumentReader()!;
	const session = useDocumentReadingStore(
		(state) => state.sessions[reader.key],
	);
	useEffect(() => {
		const state = useDocumentReadingStore.getState();
		if (reader.connection.kind === "local") state.register(reader);
		return () => useDocumentReadingStore.getState().unregister(reader.key);
	}, [reader]);
	return session && (session.opening || session.history.length) ? (
		<DocumentSheetReader {...props} />
	) : null;
}
function DocumentSheetReader({
	surfaceRef,
	composerRef,
}: {
	surfaceRef: RefObject<HTMLElement | null>;
	composerRef: RefObject<HTMLElement | null>;
}) {
	const reader = useDocumentReader()!;
	const { t } = useTranslation();
	const bounds = useConversationFrameSafeRect(surfaceRef, composerRef);
	const session = useDocumentReadingStore(
		(state) => state.sessions[reader.key],
	);
	const visit = session?.history[session.index];
	const opening = session?.opening;
	const entry = useRef<HTMLButtonElement>(null);
	const restore = useRef<HTMLElement | null>(null);
	const id = useId();
	const preferenceKey = conversationFrameSessionKey(
		reader.sessionId,
		reader.connection.kind === "remote"
			? reader.connection.identity
			: undefined,
	);
	const size = useConversationFrameStore(
		(state) => state.documentSizes[preferenceKey] ?? defaultSize,
	);
	const setSize = useConversationFrameStore((state) => state.setDocumentSize);
	const [downloading, setDownloading] = useState(false);
	const [preview, setPreview] = useState<{ width?: number; height?: number }>(
		{},
	);

	useEffect(() => {
		const surface = surfaceRef.current;
		if (!surface) return;
		const foreground = () =>
			useDocumentReadingStore.getState().close(reader.key);
		surface.addEventListener("ao:frame-foreground", foreground);
		return () => surface.removeEventListener("ao:frame-foreground", foreground);
	}, [reader.key, surfaceRef]);
	const requestId = opening?.status === "loading" ? opening.id : undefined;
	useEffect(() => {
		if (opening?.status !== "loading") return;
		const abort = new AbortController();
		void resolveDocument(reader.connection, opening.input, abort.signal).then(
			(document) =>
				useDocumentReadingStore
					.getState()
					.complete(reader.key, opening.id, document),
			(error) => {
				if (!abort.signal.aborted)
					useDocumentReadingStore
						.getState()
						.fail(
							reader.key,
							opening.id,
							error instanceof DocumentReadError
								? error.code
								: "DOCUMENT_UNAVAILABLE",
							error instanceof Error ? error.message : "",
						);
			},
		);
		return () => abort.abort();
	}, [requestId, reader]); // input is immutable for each request id
	const open = Boolean(session?.foreground && (opening || visit));
	useEffect(() => {
		if (open)
			restore.current =
				document.activeElement instanceof HTMLElement
					? document.activeElement
					: null;
	}, [open]);
	useEffect(
		() =>
			aoBridge.app.onDocumentEscape?.(() => {
				if (
					document.activeElement instanceof HTMLIFrameElement &&
					document.activeElement.hasAttribute("data-document-static")
				)
					useDocumentReadingStore.getState().close(reader.key);
			}),
		[reader.key],
	);
	const metadata = useQuery({
		queryKey: [
			"document-change",
			reader.connection.identity,
			visit ? documentFileKey(visit.document.target) : null,
		],
		queryFn: ({ signal }) =>
			resolveDocument(reader.connection, visit!.document.target, signal),
		enabled: open && Boolean(visit) && !opening,
		refetchInterval: 5000,
		retry: false,
	});
	const close = () => useDocumentReadingStore.getState().close(reader.key);
	const copy = async (
		kind: "path" | "link",
		input: DocumentInput | undefined = visit?.document.target,
	) => {
		if (!input) return;
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
	const control = (
		label: string,
		action: () => void,
		icon: React.ReactNode,
		disabled = false,
	) => (
		<Button
			variant="ghost"
			size="icon-sm"
			aria-label={label}
			title={label}
			onClick={action}
			disabled={disabled}
		>
			{icon}
		</Button>
	);
	const content = (
		<div className="flex h-full min-h-0 flex-col">
			<div className="flex shrink-0 flex-wrap items-center gap-1 border-b pb-2">
				{control(
					t("documents.sheet.back"),
					() => useDocumentReadingStore.getState().back(reader.key),
					<ArrowLeft className="size-4" />,
					!session || session.index <= 0,
				)}
				{control(
					t("documents.sheet.forward"),
					() => useDocumentReadingStore.getState().forward(reader.key),
					<ArrowRight className="size-4" />,
					!session || session.index >= session.history.length - 1,
				)}
				{visit && !opening ? (
					<>
						<Button
							variant="ghost"
							size="sm"
							aria-pressed={visit.mode === "preview"}
							onClick={() =>
								useDocumentReadingStore
									.getState()
									.setMode(reader.key, "preview")
							}
						>
							{t("documents.sheet.preview")}
						</Button>
						<Button
							variant="ghost"
							size="sm"
							aria-pressed={visit.mode === "source"}
							onClick={() =>
								useDocumentReadingStore.getState().setMode(reader.key, "source")
							}
						>
							{t("documents.sheet.source")}
						</Button>
						{control(
							t("documents.sheet.zoomOut"),
							() =>
								useDocumentReadingStore.getState().savePosition(reader.key, {
									...visit.position,
									zoom: Math.max(0.5, visit.position.zoom - 0.25),
								}),
							<ZoomOut className="size-4" />,
						)}
						<Button
							variant="ghost"
							size="sm"
							onClick={() =>
								useDocumentReadingStore
									.getState()
									.savePosition(reader.key, { ...visit.position, zoom: 1 })
							}
						>
							{t("documents.sheet.fit")}
						</Button>
						{control(
							t("documents.sheet.zoomIn"),
							() =>
								useDocumentReadingStore.getState().savePosition(reader.key, {
									...visit.position,
									zoom: Math.min(3, visit.position.zoom + 0.25),
								}),
							<ZoomIn className="size-4" />,
						)}
						{control(
							t("documents.sheet.download"),
							async () => {
								setDownloading(true);
								try {
									await downloadDocument(
										reader.connection,
										visit.document.target,
									);
								} catch {
									useUiStore
										.getState()
										.showGlobalToast(
											t("documents.sheet.downloadError"),
											undefined,
											{ tone: "error" },
										);
								} finally {
									setDownloading(false);
								}
							},
							<Download className="size-4" />,
							downloading ||
								(visit.document.target.kind === "artifact" &&
									visit.document.size > 50 * 1024 * 1024),
						)}
						{control(
							t("documents.sheet.copyPath"),
							() => void copy("path"),
							<Copy className="size-4" />,
						)}
						<Button variant="ghost" size="sm" onClick={() => void copy("link")}>
							{t("documents.sheet.copyLink")}
						</Button>
						{control(
							t("documents.sheet.refresh"),
							() =>
								useDocumentReadingStore
									.getState()
									.open(reader.key, visit.document.target),
							<RefreshCw className="size-4" />,
						)}
						{visit.document.target.kind === "artifact" ? (
							<DocumentSourceButton
								projectId={visit.document.target.projectId}
								sessionId={visit.document.target.sessionId}
							/>
						) : null}
					</>
				) : null}
			</div>
			{opening ? (
				opening.status === "loading" ? (
					<p role="status" className="p-3 text-sm">
						{t("documents.sheet.loading")}
					</p>
				) : (
					<div
						role={
							opening.code === "DOCUMENT_SOURCE_MISSING" ? "status" : "alert"
						}
						className="p-3 text-sm"
					>
						<p>{t(documentErrorKey(opening))}</p>
						<Button
							variant="ghost"
							onClick={() =>
								useDocumentReadingStore
									.getState()
									.open(reader.key, opening.input)
							}
						>
							{t(
								opening.code === "DOCUMENT_SOURCE_MISSING"
									? "documents.sheet.refresh"
									: "documents.sheet.retry",
							)}
						</Button>
						<Button
							variant="ghost"
							size="sm"
							onClick={() => void copy("link", opening.input)}
						>
							{t("documents.sheet.copyLink")}
						</Button>
					</div>
				)
			) : visit ? (
				<>
					<div
						className="min-w-0 shrink-0 border-b py-2 font-mono text-xs text-muted-foreground"
						title={visit.document.localPath}
					>
						<p className="truncate">{visit.document.localPath}</p>
						<div className="truncate">
							{t("documents.sheet.metadata", {
								size: visit.document.size,
								media: visit.document.mediaType,
							})}{" "}
							· {visit.document.target.kind} · {visit.document.target.projectId}
							{visit.document.target.kind === "artifact"
								? ` / ${visit.document.target.sessionId}`
								: ""}
						</div>
					</div>
					{metadata.data &&
					metadata.data.fingerprint !== visit.document.fingerprint ? (
						<p role="status" className="text-xs">
							{t("documents.sheet.changed")}
							<Button
								variant="ghost"
								size="sm"
								onClick={() =>
									useDocumentReadingStore
										.getState()
										.open(reader.key, visit.document.target)
								}
							>
								{t("documents.sheet.refresh")}
							</Button>
						</p>
					) : null}
					{metadata.isError ? (
						<p role="status" className="text-xs">
							{t(documentErrorKey(metadata.error))}
						</p>
					) : null}
					<DocumentBody
						key={documentFileKey(visit.document.target)}
						visit={visit}
						readerKey={reader.key}
						connection={reader.connection}
					/>
				</>
			) : null}
		</div>
	);
	return (
		<>
			{session && (visit || opening) ? (
				<Button
					ref={entry}
					variant="ghost"
					size="sm"
					aria-expanded={open}
					aria-controls={id}
					onClick={() => useDocumentReadingStore.getState().reveal(reader.key)}
				>
					{t("documents.sheet.title")}
				</Button>
			) : null}
			{bounds ? (
				<Sheet
					modal={false}
					open={open && bounds.rect.width >= 100 && bounds.rect.height >= 40}
					onOpenChange={(value) => {
						if (!value) close();
					}}
				>
					<SheetContent
						data-conversation-nonmodal=""
						showCloseButton={false}
						aria-describedby={undefined}
						className="pointer-events-none gap-0 border-0 bg-transparent p-0 shadow-none transition-none"
						style={{
							inset: "auto",
							left:
								bounds.surface.getBoundingClientRect().left + bounds.rect.left,
							top: bounds.surface.getBoundingClientRect().top + bounds.rect.top,
							width: bounds.rect.width,
							height: bounds.rect.height,
							maxWidth: "none",
							zIndex: 30,
						}}
						onInteractOutside={(event) => event.preventDefault()}
						onOpenAutoFocus={(event) => {
							event.preventDefault();
							document.getElementById(id)?.focus();
						}}
						onCloseAutoFocus={(event) => {
							event.preventDefault();
							const target = restore.current?.isConnected
								? restore.current
								: entry.current;
							target?.focus();
						}}
					>
						<SheetTitle className="sr-only">
							{t("documents.sheet.title")}
						</SheetTitle>
						<ResizableConversationFrame
							scrollContent={false}
							id={id}
							frame={{
								title: opening
									? t("documents.sheet.loading")
									: (visit?.document.target.path.split("/").at(-1) ??
										t("documents.sheet.title")),
								summary: session
									? `${session.index + 1}/${session.history.length}`
									: "",
								content,
							}}
							limits={limits}
							rect={{
								left: 0,
								top: 0,
								width: Math.min(preview.width ?? size.width, bounds.rect.width),
								height: Math.min(
									preview.height ?? size.height,
									bounds.rect.height,
								),
							}}
							safeRect={{
								left: 0,
								top: 0,
								width: bounds.rect.width,
								height: bounds.rect.height,
							}}
							maxHeight={bounds.rect.height}
							onClose={close}
							onSize={(value) => setSize(preferenceKey, value)}
							onPreview={(axis, value) =>
								setPreview((previous) => ({
									...previous,
									[axis === "x" ? "width" : "height"]: value ?? undefined,
								}))
							}
						/>
					</SheetContent>
				</Sheet>
			) : null}
		</>
	);
}

function DocumentSourceButton({
	projectId,
	sessionId,
}: {
	projectId: string;
	sessionId: string;
}) {
	const navigate = useNavigateToSession();
	const { t } = useTranslation();
	return (
		<Button
			variant="ghost"
			size="icon-sm"
			aria-label={t("documents.sheet.goSource")}
			title={t("documents.sheet.goSource")}
			onClick={() => navigate(projectId, sessionId)}
		>
			<ArrowUpRight className="size-4" />
		</Button>
	);
}
