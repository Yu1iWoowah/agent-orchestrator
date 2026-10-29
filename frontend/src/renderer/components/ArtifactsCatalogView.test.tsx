import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import type { components } from "../../api/schema";
import { ArtifactsView } from "./ArtifactsView";
import { TooltipProvider } from "./ui/tooltip";
import { useArtifactsViewStore } from "../stores/artifacts-view-store";
import { useDocumentReadingStore } from "../stores/document-reading-store";
const mocks = vi.hoisted(() => ({ fetch: vi.fn(), read: vi.fn(), resolve: vi.fn(), navigate: vi.fn() }));
vi.mock("../hooks/useWorkspaceQuery", () => ({
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
vi.mock("../lib/document-read", async (original) => ({
	...(await original<typeof import("../lib/document-read")>()),
	fetchDocumentCatalog: mocks.fetch,
	resolveDocument: mocks.resolve,
	readDocumentContent: mocks.read,
}));
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
	mocks.read.mockReset().mockResolvedValue(new TextEncoder().encode("# Shared document").buffer);
	mocks.resolve
		.mockReset()
		.mockImplementation(async (_connection, target) => ({
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
it("shows uninitialized shared as not created, and never invents a zero total", async () => {
	mocks.fetch.mockResolvedValue({
		items: [],
		errors: [{ source: "shared", code: "DOCUMENT_SOURCE_MISSING" }],
		incomplete: true,
		revision: "v1",
	});
	const view = setup();
	await screen.findByText("Total unavailable");
	expect(screen.getAllByText(/Not created yet/)).toHaveLength(2);
	expect(screen.queryByRole("alert")).toBeNull();
	expect(screen.queryByText("0 files loaded")).toBeNull();
	view.unmount();
	view.cache.clear();
});
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
	await userEvent.click(screen.getByRole("menuitem", { name: "Go to source" }));
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
		return new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError")), { once: true }));
	});
	function SidebarObserver() { useArtifactCatalogs([{ id: "p", name: "P", path: "/registered/p", sessions: [] }, { id: "bad", name: "Bad", path: "/registered/bad", sessions: [] }], false); return null; }
	const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const content = (open: boolean) => <QueryClientProvider client={cache}><TooltipProvider><SidebarObserver />{open ? <ArtifactsView /> : null}</TooltipProvider></QueryClientProvider>;
	const view = render(content(true)); await waitFor(() => expect(received).toBeDefined());
	view.rerender(content(false)); await waitFor(() => expect(received?.aborted).toBe(true)); view.unmount(); cache.clear();
});
