import * as Dialog from "@radix-ui/react-dialog";
import { useQuery } from "@tanstack/react-query";
import { Download, RefreshCw, X } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import Markdown, { defaultUrlTransform } from "react-markdown";
import rehypeSlug from "rehype-slug";
import remarkGfm from "remark-gfm";
import { apiClient, apiErrorMessage, getApiBaseUrl } from "../lib/api-client";
import { mediaCrossOrigin } from "../lib/file-media";
import {
	isAoLink,
	projectDocumentLink,
	relativeProjectDocumentPath,
	relativeProjectDocumentLink,
	type ProjectDocumentTarget,
} from "../lib/session-links";
import { useSessionLinkNavigation } from "../lib/use-session-link-navigation";
import { useUiStore } from "../stores/ui-store";
import { Button } from "./ui/button";
import "github-markdown-css/github-markdown.css";

function fileUrl(target: ProjectDocumentTarget, path = target.path): string {
	return `${getApiBaseUrl()}/api/v1/projects/${encodeURIComponent(target.projectId)}/documents/files/${path.split("/").map(encodeURIComponent).join("/")}`;
}

export function ProjectDocumentViewer() {
	const target = useUiStore((state) => state.projectDocumentTarget);
	const setTarget = useUiStore((state) => state.setProjectDocumentTarget);
	return (
		<Dialog.Root
			open={target !== null}
			onOpenChange={(open) => {
				if (!open) setTarget(null);
			}}
		>
			{target && <ProjectDocumentContent key={`${target.projectId}/${target.path}`} target={target} />}
		</Dialog.Root>
	);
}

function ProjectDocumentContent({ target }: { target: ProjectDocumentTarget }) {
	const { t } = useTranslation();
	const [raw, setRaw] = useState(false);
	const [downloadError, setDownloadError] = useState<string | null>(null);
	const [downloading, setDownloading] = useState(false);
	const navigateLink = useSessionLinkNavigation();
	const query = useQuery({
		queryKey: ["project-document", target.projectId, target.path],
		staleTime: 0,
		retry: false,
		queryFn: async ({ signal }) => {
			const { data, error } = await apiClient.GET("/api/v1/projects/{id}/documents/file", {
				params: { path: { id: target.projectId }, query: { path: target.path } },
				signal,
			});
			if (error) throw new Error(`${error.code}: ${apiErrorMessage(error)}`);
			if (!data) throw new Error("Document response is missing");
			return data.file;
		},
	});
	const download = async () => {
		setDownloading(true);
		setDownloadError(null);
		try {
			const { data, error } = await apiClient.GET("/api/v1/projects/{id}/documents/files/*", {
				params: { path: { id: target.projectId } },
				pathSerializer: () =>
					`/api/v1/projects/${encodeURIComponent(target.projectId)}/documents/files/${target.path.split("/").map(encodeURIComponent).join("/")}`,
				parseAs: "blob",
			});
			if (error) throw new Error(`${error.code}: ${apiErrorMessage(error)}`);
			if (!data) throw new Error("Download response is missing");
			const url = URL.createObjectURL(data);
			const a = document.createElement("a");
			a.href = url;
			a.download = target.path.split("/").at(-1) ?? "document";
			document.body.append(a);
			a.click();
			a.remove();
			setTimeout(() => URL.revokeObjectURL(url), 1_000);
		} catch (error) {
			setDownloadError(error instanceof Error ? error.message : String(error));
		} finally {
			setDownloading(false);
		}
	};
	const file = query.data;
	const [content, setContent] = useState<HTMLDivElement | null>(null);
	useEffect(() => {
		if (!file || !content || raw || target.fragment === undefined) return;
		if (target.fragment === "") {
			content.scrollTo({ top: 0 });
			return;
		}
		const heading = Array.from(content.querySelectorAll("[id]")).find(
			(element) => element.id === `project-document-${target.fragment}`,
		);
		heading?.scrollIntoView({ block: "start" });
	}, [content, file, raw, target]);
	const markdown = /\.md(?:own|arkdown)?$/i.test(target.path);
	return (
		<Dialog.Portal>
			<Dialog.Overlay className="dialog-overlay" />
			<Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex h-[85vh] w-[min(90vw,64rem)] -translate-x-1/2 -translate-y-1/2 flex-col rounded-xl border border-border bg-background shadow-2xl">
				<div className="flex items-center gap-2 border-b border-border p-4">
					<div className="min-w-0 flex-1">
						<Dialog.Title className="truncate font-medium">{target.path}</Dialog.Title>
						<Dialog.Description className="text-xs text-muted-foreground">
							{t("projectDocuments.readOnly", { defaultValue: "Shared project document · Read only" })} ·{" "}
							{target.projectId}
						</Dialog.Description>
					</div>
					{markdown && file && !file.binary && (
						<Button variant="outline" size="sm" onClick={() => setRaw(!raw)}>
							{raw
								? t("projectDocuments.preview", { defaultValue: "Preview" })
								: t("projectDocuments.source", { defaultValue: "Source" })}
						</Button>
					)}
					<Button
						variant="ghost"
						size="icon"
						onClick={() => void query.refetch()}
						aria-label={t("projectDocuments.refresh", { defaultValue: "Refresh document" })}
					>
						<RefreshCw className="size-4" />
					</Button>
					<Button
						variant="ghost"
						size="icon"
						disabled={downloading}
						onClick={() => void download()}
						aria-label={t("projectDocuments.download", { defaultValue: "Download document" })}
					>
						<Download className="size-4" />
					</Button>
					<Dialog.Close asChild>
						<Button
							variant="ghost"
							size="icon"
							aria-label={t("projectDocuments.close", { defaultValue: "Close document" })}
						>
							<X className="size-4" />
						</Button>
					</Dialog.Close>
				</div>
				<div ref={setContent} className="min-h-0 flex-1 overflow-auto p-6">
					{query.isPending && (
						<p role="status">{t("projectDocuments.loading", { defaultValue: "Loading document…" })}</p>
					)}
					{downloadError && (
						<p role="alert" className="text-destructive">
							{downloadError}
						</p>
					)}
					{query.error && (
						<p role="alert" className="whitespace-pre-wrap text-destructive">
							{query.error.message}
						</p>
					)}
					{file?.binary && (
						<p>
							{t("projectDocuments.binary", {
								defaultValue: "This file is binary. Download it to preserve its original bytes.",
							})}
						</p>
					)}
					{file &&
						!file.binary &&
						(markdown && !raw ? (
							<div className="markdown-body bg-transparent">
								<Markdown
									remarkPlugins={[remarkGfm]}
									rehypePlugins={[[rehypeSlug, { prefix: "project-document-" }]]}
									urlTransform={(url) => (isAoLink(url) ? url : defaultUrlTransform(url))}
									components={{
										a: ({ href, children }) => {
											if (!href) return <>{children}</>;
											const relative = relativeProjectDocumentLink(target.path, href);
											const link = relative
												? projectDocumentLink(target.projectId, relative.path, relative.fragment)
												: href;
											return isAoLink(link) ? (
												<a
													href={link}
													onClick={(event) => {
														event.preventDefault();
														navigateLink(link);
													}}
												>
													{children}
												</a>
											) : (
												<span title={href}>{children}</span>
											);
										},
										img: ({ src, alt }) => {
											const relative =
												typeof src === "string" ? relativeProjectDocumentPath(target.path, src) : undefined;
											return relative ? (
												<img
													src={fileUrl(target, relative)}
													crossOrigin={mediaCrossOrigin(fileUrl(target, relative))}
													alt={alt ?? ""}
												/>
											) : (
												<span>{alt}</span>
											);
										},
									}}
								>
									{file.content}
								</Markdown>
							</div>
						) : (
							<pre className="whitespace-pre-wrap break-words font-mono text-sm">{file.content}</pre>
						))}
				</div>
				{file && (
					<div className="break-all border-t border-border px-4 py-2 font-mono text-xs text-muted-foreground">
						{t("projectDocuments.digest", {
							defaultValue: "SHA-256: {{hash}} · {{size}} bytes",
							hash: file.hash,
							size: file.size,
						})}
					</div>
				)}
			</Dialog.Content>
		</Dialog.Portal>
	);
}
