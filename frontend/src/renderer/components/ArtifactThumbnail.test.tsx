import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ArtifactThumbnail } from "./ArtifactThumbnail";
import type { ArtifactRow } from "../lib/artifacts";
vi.mock("./PdfPreview", () => ({
	PdfPreview: ({ src, firstPageOnly }: { src: string; firstPageOnly?: boolean }) => (
		<div data-testid="pdf" data-src={src} data-first={firstPageOnly} />
	),
}));
vi.mock("./chat/MermaidBlock", () => ({
	MermaidBlock: ({ code }: { code: string }) => <pre data-testid="mermaid">{code}</pre>,
}));
beforeEach(() => {
	vi.stubGlobal(
		"IntersectionObserver",
		class {
			constructor(private callback: (entries: { isIntersecting: boolean }[]) => void) {}
			observe() {
				this.callback([{ isIntersecting: true }]);
			}
			disconnect() {}
		},
	);
	vi.stubGlobal(
		"ResizeObserver",
		class {
			observe() {}
			disconnect() {}
		},
	);
});
afterEach(() => vi.unstubAllGlobals());
function setup(path: string, kind: ArtifactRow["kind"] = "file", previewLive = true) {
	const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const row: ArtifactRow = {
		path,
		name: path,
		sessionId: "s",
		sessionTitle: "Inspector",
		projectId: "p",
		kind,
		generating: false,
		updatedAt: "2026-10-11T10:00:00Z",
		rawUrl: `http://127.0.0.1:3002/${path}`,
		inlineUrl: `http://ao-inline-artifact.s.localhost:3002/${path}`,
	};
	const view = render(
		<QueryClientProvider client={cache}>
			<ArtifactThumbnail row={row} previewLive={previewLive} />
		</QueryClientProvider>,
	);
	return { ...view, cache, row };
}
it("keeps Inspector HTML confined to its dedicated origin and probes availability", async () => {
	const fetch = vi.fn().mockResolvedValue(new Response("<h1>Inspector</h1>"));
	vi.stubGlobal("fetch", fetch);
	const view = setup("page.html", "html");
	await waitFor(() => expect(view.container.querySelector("iframe")).toHaveAttribute("src", view.row.inlineUrl));
	expect(view.container.querySelector("iframe")).toHaveAttribute("sandbox", "allow-scripts allow-same-origin");
	expect(fetch).toHaveBeenCalledTimes(1);
	view.unmount();
	view.cache.clear();
});
it.each(["missing", "network"])("keeps Inspector unavailable HTML as a placeholder: %s", async (failure) => {
	const fetch = vi.fn();
	failure === "missing"
		? fetch.mockResolvedValue(new Response("missing", { status: 404 }))
		: fetch.mockRejectedValue(new TypeError("offline"));
	vi.stubGlobal("fetch", fetch);
	const view = setup("page.html", "html");
	await screen.findByTitle("No preview available");
	expect(view.container.querySelector("iframe")).toBeNull();
	view.unmount();
	view.cache.clear();
});
it("honors the Inspector live-preview limit without reading inactive HTML", () => {
	const fetch = vi.fn();
	vi.stubGlobal("fetch", fetch);
	const view = setup("page.html", "html", false);
	expect(view.container.querySelector("iframe")).toBeNull();
	expect(fetch).not.toHaveBeenCalled();
	view.unmount();
	view.cache.clear();
});
it.each([
	["metrics.csv", "Name,Value\nAlpha,42", "Alpha", "table"],
	["metrics.tsv", "Name\tValue\nAlpha\t42", "Alpha", "table"],
	["flow.mmd", "flowchart LR\n A --> B", "A --> B", "pre"],
	["architecture.md", "```mermaid\nflowchart LR\n A --> B\n```", "A --> B", "pre"],
])("keeps Inspector %s previews in the shared renderer", async (path, content, text, selector) => {
	vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(content)));
	const view = setup(path, path.endsWith(".md") ? "markdown" : "file");
	await waitFor(() => expect(view.container.textContent).toContain(text));
	expect(view.container.querySelector(selector)).not.toBeNull();
	expect(view.container.querySelector("[inert]")).not.toBeNull();
	view.unmount();
	view.cache.clear();
});
it("keeps Inspector Markdown inert without active links or embedded HTML", async () => {
	vi.stubGlobal(
		"fetch",
		vi.fn().mockResolvedValue(new Response("# Findings\n\n[Read](https://example.com)\n\n<script>alert(1)</script>")),
	);
	const view = setup("research.md", "markdown");
	await screen.findByText("Findings");
	expect(view.container.querySelector("a, script")).toBeNull();
	view.unmount();
	view.cache.clear();
});
it("keeps Inspector first-page PDF preview and file version", () => {
	const view = setup("report.pdf");
	expect(screen.getByTestId("pdf")).toHaveAttribute("data-first", "true");
	expect(screen.getByTestId("pdf")).toHaveAttribute("data-src", view.row.rawUrl + "?v=2026-10-11T10%3A00%3A00Z");
	view.unmount();
	view.cache.clear();
});
it("keeps Inspector image failure fallback", () => {
	const view = setup("cover.png");
	const image = view.container.querySelector("img");
	if (!image) throw new Error("Missing image");
	expect(image).toHaveAttribute("src", view.row.rawUrl);
	fireEvent.error(image);
	expect(view.container.querySelector("img")).toBeNull();
	expect(screen.getByTitle("No preview available")).toBeInTheDocument();
	view.unmount();
	view.cache.clear();
});
