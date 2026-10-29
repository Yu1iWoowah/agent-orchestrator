import { DocumentCode } from "./DocumentCode";
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import Markdown, { defaultUrlTransform } from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeSlug from "rehype-slug";
import {
	classifyDocument,
	DOCUMENT_TEXT_LIMIT,
	DOCUMENT_MEDIA_LIMIT,
	DocumentContentError,
} from "../lib/document-content";
import {
	createDocumentResources,
	staticDocumentHtml,
	type DocumentResources,
} from "../lib/document-html";
import {
	documentFileKey,
	relativeDocumentTarget,
} from "../lib/document-targets";
import {
	readDocumentContent,
	resolveDocument,
	type DocumentConnection,
} from "../lib/document-read";
import {
	useDocumentReadingStore,
	type DocumentVisit,
} from "../stores/document-reading-store";
import { DocumentLink } from "./DocumentLink";
import { PdfPreview } from "./PdfPreview";
import { Button } from "./ui/button";
import {
	openLinkInSystemBrowser,
	isWebLink,
} from "../lib/external-link-policy";
import "github-markdown-css/github-markdown.css";
export function documentErrorKey(error: unknown) {
	const code =
		error && typeof error === "object" && "code" in error ? error.code : "";
	return code === "DOCUMENT_SOURCE_MISSING"
		? "documents.sheet.notCreated"
		: code === "DOCUMENT_TOO_LARGE"
			? "documents.sheet.tooLarge"
			: code === "DOCUMENT_IMAGE_PIXELS" || code === "DOCUMENT_IMAGE_INVALID"
				? "documents.sheet.unsafeImage"
				: code === "DOCUMENT_LOCAL_ONLY"
					? "documents.sheet.connection"
					: "documents.sheet.unavailable";
}
export function DocumentBody({
	compact = false,
	visit,
	readerKey,
	connection,
}: {
	compact?: boolean;
	visit: DocumentVisit;
	readerKey: string;
	connection: DocumentConnection;
}) {
	const { t } = useTranslation();
	const scroll = useRef<HTMLDivElement>(null);
	const document = visit.document;
	const query = useQuery({
		queryKey: [
			"document-content",
			connection.kind,
			connection.identity,
			documentFileKey(document.target),
			document.fingerprint,
		],
		queryFn: ({ signal }) => {
			const limit =
				/^(?:text\/|application\/(?:json|javascript|xml)|image\/svg\+xml)/i.test(
					document.mediaType,
				) || /\.(?:md|markdown|html?|svg)$/i.test(document.target.path)
					? DOCUMENT_TEXT_LIMIT
					: DOCUMENT_MEDIA_LIMIT;
			if (document.size > limit)
				throw new DocumentContentError("DOCUMENT_TOO_LARGE");
			return readDocumentContent(
				connection,
				document.target,
				limit,
				document.fingerprint,
				signal,
			);
		},
		staleTime: Infinity,
		gcTime: 120000,
		retry: false,
	});
	const resources = useMemo(
		() =>
			createDocumentResources(
				document.target,
				async (target, { maxBytes, signal }) => {
					const meta = await resolveDocument(connection, target, signal);
					if (meta.size > maxBytes)
						throw new DocumentContentError(
							meta.size > 10 * 1024 * 1024
								? "DOCUMENT_TOO_LARGE"
								: "DOCUMENT_RESOURCE_LIMIT",
						);
					return readDocumentContent(
						connection,
						meta.target,
						maxBytes,
						meta.fingerprint,
						signal,
					);
				},
				/\.html?$/i.test(document.target.path) ? "inline-raster" : "blob",
			),
		[connection, document.target, document.fingerprint],
	);
	useEffect(() => () => resources.dispose(), [resources]);
	const parsed = useMemo(() => {
		try {
			return {
				content: query.data
					? classifyDocument(document.target.path, new Uint8Array(query.data))
					: undefined,
				error: null,
			};
		} catch (error) {
			return { content: undefined, error };
		}
	}, [query.data, document.target.path]);
	const content = parsed.content;
	const error = parsed.error ?? query.error;
	useLayoutEffect(() => {
		if (!query.data || !scroll.current || content?.kind === "pdf") return;
		const frame = requestAnimationFrame(() => {
			const root = scroll.current;
			if (!root) return;
			const fragment = visit.revealFragment
				? visit.document.target.fragment
				: undefined;
			const heading = fragment
				? Array.from(root.querySelectorAll<HTMLElement>("[id]")).find(
						(item) => item.id === fragment,
					)
				: null;
			root.scrollTop =
				content?.kind === "html" && visit.mode === "preview"
					? 0
					: heading
						? root.scrollTop +
							heading.getBoundingClientRect().top -
							root.getBoundingClientRect().top
						: fragment === ""
							? 0
							: visit.position.scrollTop;
		});
		return () => cancelAnimationFrame(frame);
	}, [
		query.data,
		content?.kind,
		visit.epoch,
		visit.document.target.fragment,
		visit.mode,
		visit.revealFragment,
	]);
	if (error)
		return (
			<p role="alert" className="p-3 text-sm">
				{t(documentErrorKey(error))}
				<Button variant="ghost" size="sm" onClick={() => void query.refetch()}>
					{t("documents.sheet.retry")}
				</Button>
			</p>
		);
	if (!content)
		return (
			<p role="status" className="p-3 text-sm">
				{t("documents.sheet.loading")}
			</p>
		);
	return (
		<div
			ref={scroll}
			className={
				compact
					? "board-scrollbar min-h-0 flex-1 overflow-auto overscroll-contain p-1"
					: "board-scrollbar min-h-0 flex-1 overflow-auto overscroll-contain p-3"
			}
			data-document-scroll=""
			onScroll={(event) =>
				content.kind !== "pdf" &&
				useDocumentReadingStore.getState().savePosition(readerKey, {
					...visit.position,
					scrollTop: event.currentTarget.scrollTop,
				})
			}
		>
			{content.kind === "binary" ? (
				<p>{t("documents.sheet.binary")}</p>
			) : content.kind === "pdf" ? (
				<DocumentPdf
					data={query.data!}
					path={document.target.path}
					zoom={visit.position.zoom}
					scrollTop={visit.position.scrollTop}
					onScroll={(scrollTop) =>
						useDocumentReadingStore
							.getState()
							.savePosition(readerKey, { ...visit.position, scrollTop })
					}
				/>
			) : content.kind === "image" ? (
				<DocumentRaster
					data={query.data!}
					mime={content.mime}
					alt={document.target.path}
					zoom={visit.position.zoom}
				/>
			) : visit.mode === "source" ||
			  content.kind === "source" ||
			  content.kind === "text" ? (
				<DocumentCode
					text={"text" in content ? content.text : ""}
					path={document.target.path}
					zoom={visit.position.zoom}
				/>
			) : content.kind === "html" ? (
				<DocumentHtml
					text={content.text}
					resources={resources}
					base={document.target}
				/>
			) : (
				<div
					className="document-markdown markdown-body min-w-0 break-words"
					style={{ fontSize: `${visit.position.zoom}em` }}
				>
					<Markdown
						remarkPlugins={[remarkGfm]}
						rehypePlugins={[rehypeSlug]}
						urlTransform={(url, key) =>
							key === "href" &&
							(relativeDocumentTarget(document.target, url) || isWebLink(url))
								? url
								: key === "src"
									? url
									: defaultUrlTransform(url)
						}
						components={{
							a: ({ href, children }) => {
								const input = href
									? relativeDocumentTarget(document.target, href)
									: null;
								return input ? (
									<DocumentLink href={href} input={input}>
										{children}
									</DocumentLink>
								) : href && isWebLink(href) ? (
									<Button
										variant="ghost"
										size="sm"
										onClick={() => void openLinkInSystemBrowser(href)}
									>
										{children}
									</Button>
								) : (
									<span>{children}</span>
								);
							},
							img: ({ src, alt }) => (
								<DocumentImage
									reference={typeof src === "string" ? src : ""}
									alt={alt ?? ""}
									resources={resources}
								/>
							),
						}}
					>
						{content.text}
					</Markdown>
				</div>
			)}
		</div>
	);
}
function useBlob(data: ArrayBuffer, mime: string) {
	const [url, setUrl] = useState<string>();
	useEffect(() => {
		const next = URL.createObjectURL(new Blob([data], { type: mime }));
		setUrl(next);
		return () => URL.revokeObjectURL(next);
	}, [data, mime]);
	return url;
}
function DocumentRaster({
	data,
	mime,
	alt,
	zoom,
}: {
	data: ArrayBuffer;
	mime: string;
	alt: string;
	zoom: number;
}) {
	const url = useBlob(data, mime);
	const [failed, setFailed] = useState(false);
	const { t } = useTranslation();
	return failed ? (
		<p role="alert">{t("documents.sheet.parseError")}</p>
	) : url ? (
		<img
			src={url}
			alt={alt}
			onError={() => setFailed(true)}
			className="h-auto max-w-none"
			style={{ width: `${zoom * 100}%` }}
		/>
	) : null;
}
function DocumentPdf({
	data,
	path,
	zoom,
	scrollTop,
	onScroll,
}: {
	data: ArrayBuffer;
	path: string;
	zoom: number;
	scrollTop: number;
	onScroll: (top: number) => void;
}) {
	const url = useBlob(data, "application/pdf");
	const { t } = useTranslation();
	const [error, setError] = useState(false);
	return error ? (
		<p role="alert">{t("documents.sheet.parseError")}</p>
	) : url ? (
		<div className="flex h-full min-h-0 flex-col">
			<PdfPreview
				src={url}
				path={path}
				safeDocument
				documentZoom={zoom}
				scrollPosition={scrollTop}
				onScrollPosition={onScroll}
				onError={() => setError(true)}
			/>
		</div>
	) : null;
}
function DocumentImage({
	reference,
	alt,
	resources,
}: {
	reference: string;
	alt: string;
	resources: DocumentResources;
}) {
	const [src, setSrc] = useState<string | null>();
	const [code, setCode] = useState<string | null>(null);
	const { t } = useTranslation();
	useEffect(() => {
		let current = true;
		setSrc(undefined);
		setCode(null);
		void resources.image(reference).then(
			(value) => {
				if (current) setSrc(value);
			},
			(error: unknown) => {
				if (current) {
					setSrc(null);
					setCode(
						error &&
							typeof error === "object" &&
							"code" in error &&
							typeof error.code === "string"
							? error.code
							: null,
					);
				}
			},
		);
		return () => {
			current = false;
		};
	}, [reference, resources]);
	return src ? (
		<img
			src={src}
			alt={alt}
			onError={() => setSrc(null)}
			className="h-auto max-w-full"
		/>
	) : (
		<span className="text-muted-foreground">
			{alt || t("documents.sheet.blockedImage")}
			{isWebLink(reference) ? (
				<Button
					variant="ghost"
					size="sm"
					onClick={() => void openLinkInSystemBrowser(reference)}
				>
					{t("documents.sheet.external")}
				</Button>
			) : null}
			{code === "DOCUMENT_TOO_LARGE"
				? ` ${t("documents.sheet.resourceTooLarge")}`
				: resources.limited() || code === "DOCUMENT_RESOURCE_LIMIT"
					? ` ${t("documents.sheet.resourceLimit")}`
					: null}
		</span>
	);
}
function DocumentHtml({
	text,
	resources,
	base,
}: {
	text: string;
	resources: DocumentResources;
	base: DocumentVisit["document"]["target"];
}) {
	const [result, setResult] =
		useState<Awaited<ReturnType<typeof staticDocumentHtml>>>();
	const [error, setError] = useState(false);
	const { t } = useTranslation();
	useEffect(() => {
		let current = true;
		setResult(undefined);
		setError(false);
		void staticDocumentHtml(text, resources).then(
			(value) => {
				if (current) setResult(value);
			},
			() => {
				if (current) setError(true);
			},
		);
		return () => {
			current = false;
		};
	}, [text, resources]);
	if (error) return <p role="alert">{t("documents.sheet.parseError")}</p>;
	return result ? (
		<>
			<p className="mb-2 text-xs text-muted-foreground">
				{t("documents.sheet.staticHtml")}
			</p>
			{resources.limited() ? (
				<p role="status" className="text-xs">
					{t("documents.sheet.resourceLimit")}
				</p>
			) : null}
			<iframe
				data-document-static=""
				title={t("documents.sheet.preview")}
				sandbox=""
				referrerPolicy="no-referrer"
				srcDoc={result.html}
				className="h-full min-h-80 w-full border-0 bg-white"
			/>
			{result.links.length ? (
				<nav
					aria-label={t("documents.sheet.links")}
					className="flex flex-col items-start"
				>
					{result.links.map((link, index) => {
						const input = relativeDocumentTarget(base, link.href);
						return input ? (
							<DocumentLink key={index} href={link.href} input={input}>
								{link.label}
							</DocumentLink>
						) : isWebLink(link.href) ? (
							<Button
								key={index}
								variant="ghost"
								onClick={() => void openLinkInSystemBrowser(link.href)}
							>
								{link.label}
							</Button>
						) : (
							<span key={index}>{link.label}</span>
						);
					})}
				</nav>
			) : null}
		</>
	) : (
		<p role="status">{t("documents.sheet.loading")}</p>
	);
}
