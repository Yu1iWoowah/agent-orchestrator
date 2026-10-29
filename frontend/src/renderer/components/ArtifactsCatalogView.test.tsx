import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import type { components } from "../../api/schema";
import { ArtifactsView } from "./ArtifactsView";
import { TooltipProvider } from "./ui/tooltip";
import { useArtifactsViewStore } from "../stores/artifacts-view-store";
import { aoBridge } from "../lib/bridge";
import { useUiStore } from "../stores/ui-store";
import { useDocumentReadingStore } from "../stores/document-reading-store";
const mocks = vi.hoisted(() => ({
	fetch: vi.fn(),
	read: vi.fn(),
	resolve: vi.fn(),
	navigate: vi.fn(),
	remove: vi.fn(),
}));
vi.mock("../hooks/useWorkspaceQuery", () => ({
	workspaceQueryKeyForHost: () => ["workspaces"],
	cloudSessionsQueryKey: ["cloud-sessions"],
	useWorkspaceQuery: () => ({
		isLoading: false,
		data: [
			{ id: "p", name: "Healthy project", path: "/registered/p", sessions: [] },
			{ id: "bad", name: "Unavailable project", path: "/registered/bad", sessions: [] },
		],
	}),
}));
vi.mock("../hooks/useCloudOffering", () => ({ useCloudOffering: () => ({ artifacts: false }) }));
vi.mock("../hooks/useCreateArtifactSession", () => ({
	useCreateArtifactSession: () => ({ create: vi.fn(), pending: false, agentReady: false }),
}));
vi.mock("../lib/navigate-to-session", () => ({ useNavigateToSession: () => mocks.navigate }));
vi.mock("../hooks/useConversationFrameSafeRect", () => ({
	useConversationFrameSafeRect: () => ({
		surface: document.body,
		rect: { left: 10, top: 50, width: 800, height: 500 },
	}),
}));
vi.mock("../lib/api-client", async (original) => ({
	...(await original<typeof import("../lib/api-client")>()),
	getApiBaseUrl: () => "http://127.0.0.1:18448",
}));
vi.mock("../lib/document-read", async (original) => ({
	...(await original<typeof import("../lib/document-read")>()),
	fetchDocumentCatalog: mocks.fetch,
	resolveDocument: mocks.resolve,
	readDocumentContent: mocks.read,
}));
vi.mock("../hooks/useSessionWorkspaceFiles", async (original) => ({
	...(await original<typeof import("../hooks/useSessionWorkspaceFiles")>()),
	deleteSessionArtifactFile: mocks.remove,
}));
afterEach(() => {
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});
const item = (
	source: "worker" | "orchestrator" | "shared",
	archived = false,
): components["schemas"]["DocumentCatalogItem"] => ({
	source,
	archived,
	name: "guide.md",
	path: `${source}/guide.md`,
	size: 10,
	fingerprint: source,
	updatedAt: source === "shared" ? undefined : "2026-10-29T00:00:00Z",
	target:
		source === "shared"
			? { kind: "shared", projectId: "p", path: "shared/guide.md" }
			: { kind: "artifact", projectId: "p", sessionId: source + "-1", path: `${source}/guide.md` },
});
beforeEach(() => {
	useArtifactsViewStore.setState({
		view: "list",
		pinnedArtifactIds: [],
		pinAliases: {},
		collapsedSections: [],
		galleryPreferences: {},
	});
	useDocumentReadingStore.setState({ sessions: {}, reader: null });
	mocks.navigate.mockReset();
	mocks.remove.mockReset().mockResolvedValue(undefined);
	useUiStore.setState({ workspaceFileOpenRequest: null });
	mocks.read.mockReset().mockResolvedValue(new TextEncoder().encode("# Shared document").buffer);
	mocks.resolve.mockReset().mockImplementation(async (_connection, target) => ({
		target,
		canonicalUri: "ao://projects/p/shared/shared/guide.md",
		localPath: "/managed/shared/guide.md",
		size: 10,
		fingerprint: "shared",
		mediaType: "text/markdown",
	}));
	mocks.fetch.mockReset().mockImplementation(async (_connection, id) => {
		if (id === "bad") throw new Error("offline");
		return {
			items: [item("worker"), item("orchestrator", true), item("shared")],
			errors: [],
			incomplete: false,
			revision: "v1",
		};
	});
});
function setup() {
	const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const view = render(
		<QueryClientProvider client={cache}>
			<TooltipProvider>
				<ArtifactsView />
			</TooltipProvider>
		</QueryClientProvider>,
	);
	return { ...view, cache };
}
it("shows all three sources and Archive by default alongside a failed project without reading file content", async () => {
	const { unmount, cache } = setup();
	await screen.findByText("shared/guide.md");
	expect(screen.getByText("worker/guide.md")).toBeVisible();
	expect(screen.getByText("orchestrator/guide.md")).toBeVisible();
	expect(screen.getByText(/Unavailable project/)).toBeVisible();
	expect(screen.getByText(/Updated time unknown/)).toBeVisible();
	expect(mocks.read).not.toHaveBeenCalled();
	expect(document.querySelector("iframe")).toBeNull();
	unmount();
	cache.clear();
});
it("filters Source and Archive and retains them across leaving the page", async () => {
	let view = setup();
	await screen.findByText("shared/guide.md");
	const user = userEvent.setup();
	await user.click(screen.getByRole("button", { name: "Filter by source" }));
	await user.click(screen.getByRole("menuitemradio", { name: /^Shared$/ }));
	expect(screen.queryByText("worker/guide.md")).toBeNull();
	fireEvent.change(screen.getByRole("searchbox", { name: "Search artifacts" }), { target: { value: "shared/" } });
	view.unmount();
	view.cache.clear();
	view = setup();
	await screen.findByText("shared/guide.md");
	expect(screen.getByRole("searchbox", { name: "Search artifacts" })).toHaveValue("shared/");
	expect(screen.queryByText("worker/guide.md")).toBeNull();
	view.unmount();
	view.cache.clear();
});
it("opens Shared in the gallery reader and gives it only read actions", async () => {
	const { unmount, cache } = setup();
	const path = await screen.findByText("shared/guide.md");
	const row = path.closest("li");
	if (!row) throw new Error("Missing row");
	const user = userEvent.setup();
	await user.click(within(row).getByRole("button", { name: /More actions/ }));
	expect(screen.queryByRole("menuitem", { name: /Delete|Restore|feedback|Go to source/i })).toBeNull();
	await user.keyboard("{Escape}");
	await user.click(within(row).getByRole("button", { name: /shared\/guide.md/ }));
	await screen.findByRole("heading", { name: "Shared document" });
	expect(mocks.navigate).not.toHaveBeenCalled();
	expect(useDocumentReadingStore.getState().reader).toMatchObject({ scope: "gallery" });
	expect(useDocumentReadingStore.getState().reader).not.toHaveProperty("sessionId");
	await waitFor(() => expect(mocks.read).toHaveBeenCalledTimes(1));
	unmount();
	cache.clear();
});
it("separates Current and Archive without treating Shared as an archived session", async () => {
	const view = setup();
	await screen.findByText("shared/guide.md");
	const user = userEvent.setup();
	await user.click(screen.getByRole("button", { name: "Filter by Archive" }));
	await user.click(screen.getByRole("menuitemradio", { name: /^Current$/ }));
	expect(screen.queryByText("orchestrator/guide.md")).toBeNull();
	expect(screen.getByText("shared/guide.md")).toBeVisible();
	await user.click(screen.getByRole("button", { name: "Filter by Archive" }));
	await user.click(screen.getByRole("menuitemradio", { name: /^Archive$/ }));
	expect(screen.getByText("orchestrator/guide.md")).toBeVisible();
	expect(screen.queryByText("worker/guide.md")).toBeNull();
	expect(screen.queryByText("shared/guide.md")).toBeNull();
	view.unmount();
	view.cache.clear();
});
it("retains a healthy page, identifies a stale cursor, and refreshes the inventory", async () => {
	mocks.fetch.mockImplementation(async (_connection, id, query) => {
		if (id === "bad") return { items: [], errors: [], incomplete: false, revision: "v1" };
		if (query.cursor)
			throw new (await import("../lib/document-read")).DocumentReadError("DOCUMENT_CATALOG_CHANGED", "expired");
		return { items: [item("worker")], errors: [], incomplete: false, revision: "v1", nextCursor: "next" };
	});
	const view = setup();
	await screen.findByText(/Files changed. Refresh to continue/);
	expect(screen.getByText("worker/guide.md")).toBeVisible();
	mocks.fetch.mockResolvedValue({ items: [], errors: [], incomplete: false, revision: "v2" });
	await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
	await waitFor(() => expect(screen.queryByText(/Files changed. Refresh to continue/)).toBeNull());
	view.unmount();
	view.cache.clear();
});
it("treats only uninitialized shared as a known empty catalog with the normal empty guidance", async () => {
	mocks.fetch.mockResolvedValue({
		items: [],
		errors: [{ source: "shared", code: "DOCUMENT_SOURCE_MISSING" }],
		incomplete: true,
		revision: "v1",
	});
	const view = setup();
	await screen.findByRole("heading", { name: "No agent is ready" });
	expect(screen.getByText("0 files loaded")).toBeVisible();
	expect(screen.queryByText("Total unavailable")).toBeNull();
	expect(screen.queryByText("Some sources could not be loaded")).toBeNull();
	expect(screen.queryByText("Not created yet")).toBeNull();
	await userEvent.click(screen.getByRole("button", { name: "Filter by source" }));
	await userEvent.click(screen.getByRole("menuitemradio", { name: /^Shared$/ }));
	expect(screen.getAllByText("Not created yet")).toHaveLength(1);
	expect(screen.queryByRole("alert")).toBeNull();
	view.unmount();
	view.cache.clear();
});
it.each(["DOCUMENT_SOURCE_UNAVAILABLE", "DOCUMENT_CATALOG_CHANGED", "request"])(
	"keeps a real %s failure unknown",
	async (code) => {
		mocks.fetch.mockImplementation(async () => {
			if (code === "request") throw new Error("offline");
			if (code === "DOCUMENT_CATALOG_CHANGED")
				throw new (await import("../lib/document-read")).DocumentReadError(code, "changed");
			return { items: [], errors: [{ source: "worker", sessionId: "failed", code }], incomplete: true, revision: "v1" };
		});
		const view = setup();
		await screen.findByText("Total unavailable");
		expect(screen.queryByText("0 files loaded")).toBeNull();
		expect(screen.queryByRole("heading", { name: "No agent is ready" })).toBeNull();
		view.unmount();
		view.cache.clear();
	},
);
it("opens a source only by its explicit menu action, and migrates/unpins old worker pins", async () => {
	mocks.fetch.mockImplementation(async (_connection, id) => ({
		items: id === "p" ? [item("worker")] : [],
		errors: [],
		incomplete: false,
		revision: "v1",
	}));
	const legacy = JSON.stringify(["worker-1", "worker/guide.md"]);
	useArtifactsViewStore.setState({ pinnedArtifactIds: [legacy] });
	const view = setup();
	await screen.findAllByText("worker/guide.md");
	const path = screen.getAllByText("worker/guide.md")[0],
		row = path.closest("li");
	if (!row) throw new Error("Missing row");
	await waitFor(() => expect(useArtifactsViewStore.getState().pinnedArtifactIds).toHaveLength(2));
	expect(useArtifactsViewStore.getState().pinnedArtifactIds).toContain(legacy);
	await userEvent.click(within(row).getByRole("button", { name: /More actions/ }));
	await userEvent.click(screen.getByRole("menuitem", { name: "Unpin artifact" }));
	expect(useArtifactsViewStore.getState().pinnedArtifactIds).toEqual([]);
	const current = screen.getByText("worker/guide.md").closest("li");
	if (!current) throw new Error("Missing unpinned row");
	await userEvent.click(within(current).getByRole("button", { name: /More actions/ }));
	await userEvent.click(screen.getByRole("menuitem", { name: "Open in session" }));
	expect(mocks.navigate).toHaveBeenCalledWith("p", "worker-1");
	expect(mocks.read).not.toHaveBeenCalled();
	view.unmount();
	view.cache.clear();
});
it("cancels a catalog on gallery exit even while the sidebar observes its cache", async () => {
	const { useArtifactCatalogs } = await import("../hooks/useArtifactCatalogs");
	let received: AbortSignal | undefined;
	mocks.fetch.mockImplementation((_connection, _id, _query, signal: AbortSignal) => {
		received = signal;
		return new Promise((_resolve, reject) =>
			signal.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")), { once: true }),
		);
	});
	function SidebarObserver() {
		useArtifactCatalogs(
			[
				{ id: "p", name: "P", path: "/registered/p", sessions: [] },
				{ id: "bad", name: "Bad", path: "/registered/bad", sessions: [] },
			],
			false,
		);
		return null;
	}
	const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const content = (open: boolean) => (
		<QueryClientProvider client={cache}>
			<TooltipProvider>
				<SidebarObserver />
				{open ? <ArtifactsView /> : null}
			</TooltipProvider>
		</QueryClientProvider>
	);
	const view = render(content(true));
	await waitFor(() => expect(received).toBeDefined());
	view.rerender(content(false));
	await waitFor(() => expect(received?.aborted).toBe(true));
	view.unmount();
	cache.clear();
});

it.each(["worker", "orchestrator"] as const)(
	"restores %s deletion with confirmation, removes the pin family and refreshes catalog rows",
	async (source) => {
		let deleted = false;
		mocks.fetch.mockImplementation(async (_connection, id) => ({
			items: id === "p" && !deleted ? [item(source)] : [],
			errors: [],
			incomplete: false,
			revision: deleted ? "v2" : "v1",
		}));
		mocks.remove.mockImplementation(async () => {
			deleted = true;
		});
		const legacy = JSON.stringify([source + "-1", source + "/guide.md"]);
		useArtifactsViewStore.setState({ pinnedArtifactIds: [legacy] });
		const view = setup();
		await screen.findByRole("region", { name: "Pinned" });
		const project = within(screen.getByRole("region", { name: "Healthy project" }));
		await userEvent.click(project.getByRole("button", { name: /More actions/ }));
		await userEvent.click(screen.getByRole("menuitem", { name: "Delete artifact" }));
		expect(mocks.remove).not.toHaveBeenCalled();
		const dialog = within(screen.getByRole("dialog"));
		expect(dialog.getByText(/guide.md/)).toBeVisible();
		await userEvent.click(dialog.getByRole("button", { name: "Delete artifact" }));
		await waitFor(() => expect(screen.queryByText(source + "/guide.md")).toBeNull());
		expect(mocks.remove).toHaveBeenCalledWith({
			sessionId: source + "-1",
			path: source + "/guide.md",
			hostId: undefined,
		});
		expect(useArtifactsViewStore.getState().pinnedArtifactIds).toEqual([]);
		expect(screen.queryByRole("region", { name: "Pinned" })).toBeNull();
		view.unmount();
		view.cache.clear();
	},
);
it("restores file manager, Terminal, Copy Markdown and opening the file in its session", async () => {
	mocks.fetch.mockImplementation(async (_connection, id) => ({
		items: id === "p" ? [item("worker")] : [],
		errors: [],
		incomplete: false,
		revision: "v1",
	}));
	const reveal = vi.spyOn(aoBridge.editorHandoff, "openArtifacts");
	const copy = vi.spyOn(aoBridge.clipboard, "writeText");
	const read = vi.fn().mockResolvedValue(new Response("# Copied worker Markdown"));
	vi.stubGlobal("fetch", read);
	const view = setup();
	await screen.findByText("worker/guide.md");
	const menu = () => userEvent.click(screen.getByRole("button", { name: /More actions/ }));
	await menu();
	await userEvent.click(screen.getByRole("menuitem", { name: /^Show in (Finder|File Manager|File Explorer)$/ }));
	await waitFor(() =>
		expect(reveal).toHaveBeenCalledWith({ sessionId: "worker-1", targetId: "file-manager", path: "worker/guide.md" }),
	);
	await menu();
	await userEvent.click(screen.getByRole("menuitem", { name: "Show in Terminal" }));
	await waitFor(() =>
		expect(reveal).toHaveBeenCalledWith({ sessionId: "worker-1", targetId: "terminal", path: "worker/guide.md" }),
	);
	await menu();
	await userEvent.click(screen.getByRole("menuitem", { name: "Copy as Markdown" }));
	await waitFor(() => expect(copy).toHaveBeenCalledWith("# Copied worker Markdown"));
	await userEvent.click(screen.getByRole("button", { name: "Open in session: guide.md" }));
	expect(mocks.navigate).toHaveBeenCalledWith("p", "worker-1");
	expect(useUiStore.getState().workspaceFileOpenRequest).toMatchObject({
		sessionId: "worker-1",
		path: "worker/guide.md",
		source: "artifact",
	});
	expect(mocks.read).not.toHaveBeenCalled();
	view.unmount();
	view.cache.clear();
});
it.each(["other project", "other session"])("migrates healthy worker pins despite a failed %s", async (failure) => {
	mocks.fetch.mockImplementation(async (_connection, id) => ({
		items: id === "p" ? [item("worker")] : [],
		errors: (failure === "other project" ? id === "bad" : id === "p")
			? [{ source: "worker", sessionId: "unrelated-worker", code: "DOCUMENT_SOURCE_UNAVAILABLE" }]
			: [],
		incomplete: failure === "other project" ? id === "bad" : id === "p",
		revision: "v1",
	}));
	const legacy = JSON.stringify(["worker-1", "worker/guide.md"]);
	useArtifactsViewStore.setState({ pinnedArtifactIds: [legacy] });
	const view = setup();
	const pinned = await screen.findByRole("region", { name: "Pinned" });
	expect(within(pinned).getByText("worker/guide.md")).toBeVisible();
	expect(useArtifactsViewStore.getState().pinnedArtifactIds).toHaveLength(2);
	expect(useArtifactsViewStore.getState().pinAliases[legacy]).toBeDefined();
	view.unmount();
	view.cache.clear();
});
it("batches scroll persistence and flushes the last position when leaving the gallery", async () => {
	const view = setup();
	await screen.findByText("shared/guide.md");
	const viewport = view.container.querySelector<HTMLElement>(".chat-scroll-viewport");
	if (!viewport) throw new Error("Missing viewport");
	const persist = vi.spyOn(window.localStorage, "setItem");
	fireEvent.wheel(viewport);
	persist.mockClear();
	for (let top = 10; top <= 200; top += 10) {
		viewport.scrollTop = top;
		fireEvent.scroll(viewport);
	}
	expect(persist).not.toHaveBeenCalled();
	view.unmount();
	expect(persist).toHaveBeenCalledTimes(1);
	const saved = JSON.parse(window.localStorage.getItem("ao.artifacts.view") ?? "{}");
	expect(Object.values(saved.galleryPreferences)).toEqual([expect.objectContaining({ scrollTop: 200 })]);
	view.cache.clear();
});

it("copies a literal percent filename through an escaped artifact read URL", async () => {
	const path = "worker/%2e%2e%2fx%.md";
	const file = {
		...item("worker"),
		name: "%2e%2e%2fx%.md",
		path,
		target: { kind: "artifact", projectId: "p", sessionId: "worker-1", path },
	};
	mocks.fetch.mockImplementation(async (_connection, id) => ({
		items: id === "p" ? [file] : [],
		errors: [],
		incomplete: false,
		revision: "v1",
	}));
	const copy = vi.spyOn(aoBridge.clipboard, "writeText");
	const read = vi.fn().mockResolvedValue(new Response("Literal filename content"));
	vi.stubGlobal("fetch", read);
	const view = setup();
	await screen.findByText(path);
	await userEvent.click(screen.getByRole("button", { name: /More actions/ }));
	await userEvent.click(screen.getByRole("menuitem", { name: "Copy as Markdown" }));
	await waitFor(() => expect(copy).toHaveBeenCalledWith("Literal filename content"));
	expect(read).toHaveBeenCalledWith(
		"http://127.0.0.1:18448/api/v1/sessions/worker-1/artifact-files/worker/%252e%252e%252fx%25.md?source=1",
	);
	view.unmount();
	view.cache.clear();
});
