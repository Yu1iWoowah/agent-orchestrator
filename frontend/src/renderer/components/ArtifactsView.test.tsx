import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useUiStore } from "../stores/ui-store";
import { useArtifactsViewStore } from "../stores/artifacts-view-store";
import {
	CLOUD_PROJECT_KIND,
	STANDALONE_WORKSPACE_ID,
	type WorkspaceSession,
	type WorkspaceSummary,
} from "../types/workspace";
import { ArtifactsView } from "./ArtifactsView";
import { TooltipProvider } from "./ui/tooltip";

const mocks = vi.hoisted(() => ({
	workspaces: [] as WorkspaceSummary[],
	agents: [] as Array<Record<string, unknown>>,
	navigate: vi.fn(),
	post: vi.fn(),
	get: vi.fn(),
	capture: vi.fn(),
	deleteCloudArtifact: vi.fn(),
	hideShellTopbar: true,
	catalog: vi.fn(),
}));

vi.mock("./PdfPreview", () => ({
	PdfPreview: ({ firstPageOnly, path, src }: { firstPageOnly?: boolean; path: string; src: string }) => (
		<div data-testid="pdf-thumbnail" data-first-page-only={firstPageOnly} data-src={src}>
			{path}
		</div>
	),
}));
vi.mock("./chat/MermaidBlock", () => ({
	MermaidBlock: ({ code }: { code: string }) => <div data-testid="mermaid-thumbnail">{code}</div>,
}));
vi.mock("@tanstack/react-router", () => ({ useNavigate: () => mocks.navigate }));
vi.mock("../hooks/useWorkspaceQuery", () => ({
	workspaceQueryKey: ["workspaces"],
	cloudSessionsQueryKey: ["cloud-sessions"],
	workspaceQueryKeyForHost: () => ["workspaces"],
	useWorkspaceQuery: () => ({ data: mocks.workspaces, isLoading: false, error: null }),
}));
vi.mock("../hooks/useCloudCp", () => ({
	useCloudCp: () => ({ client: { deleteSessionArtifact: mocks.deleteCloudArtifact } }),
}));
vi.mock("../hooks/useCloudArtifacts", async (importOriginal) => ({
	...(await importOriginal<typeof import("../hooks/useCloudArtifacts")>()),
	useCloudArtifactAccess: (session: Pick<WorkspaceSession, "cloud">) => ({
		data: session.cloud ? "https://cp.test/artifact-files/token/" : undefined,
	}),
}));
vi.mock("./CloudArtifactFrame", () => ({
	CloudArtifactFrame: ({ title, url }: { title: string; url: string }) => (
		<iframe data-testid="cloud-artifact-preview" title={title} src={url} />
	),
}));
vi.mock("./ArtifactFileView", () => ({
	ArtifactFileView: ({
		cloud,
		path,
		rawUrl,
		readOnly,
	}: {
		cloud?: boolean;
		path: string;
		rawUrl?: string;
		readOnly?: boolean;
	}) => (
		<div
			data-testid="artifact-file-preview"
			data-cloud={cloud}
			data-path={path}
			data-raw-url={rawUrl}
			data-read-only={readOnly}
		/>
	),
}));
vi.mock("../hooks/useAgentReadinessQuery", () => ({
	useAgentReadinessQuery: () => ({ data: { agents: mocks.agents } }),
}));
vi.mock("../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/api-client")>()),
	apiClient: { POST: mocks.post, GET: mocks.get },
}));
vi.mock("../lib/telemetry", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/telemetry")>()),
	captureRendererEvent: mocks.capture,
}));
vi.mock("../lib/platform", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/platform")>()),
	hidesShellTopbar: () => mocks.hideShellTopbar,
}));

vi.mock("../lib/document-read", async (original) => ({
	...(await original<typeof import("../lib/document-read")>()),
	fetchDocumentCatalog: mocks.catalog,
}));

const codexReady = {
	id: "codex",
	label: "Codex",
	installation: { state: "installed", freshness: "fresh" },
	authentication: { state: "authorized", freshness: "fresh" },
	effectiveReadiness: "ready",
	usageCount: 0,
	lastUsedAt: null,
};

function session(
	id: string,
	title: string,
	files: Array<{ name: string; updatedAt: string; kind?: "html" | "markdown" | "file" }>,
): WorkspaceSession {
	return {
		id,
		title,
		workspaceId: "ws",
		workspaceName: "ws",
		provider: "claude-code",
		kind: "worker",
		status: "idle",
		updatedAt: "2026-10-01T00:00:00Z",
		prs: [],
		outputType: files.length > 0 ? "artifact" : "none",
		artifactFiles: files.map((file) => ({
			kind: file.kind ?? "markdown",
			name: file.name,
			path: file.name,
			size: 1,
			updatedAt: file.updatedAt,
			...(file.kind === "html" ? { previewUrl: `http://preview/${file.name}` } : {}),
		})),
	} as WorkspaceSession;
}

const withArtifacts: WorkspaceSummary[] = [
	{
		id: "ao",
		name: "agent-orchestrator",
		path: "/code/ao",
		sessions: [
			session("ao-1", "Design the Artifacts entry point", [
				{ name: "artifacts-user-flow.html", updatedAt: "2026-10-11T10:00:00Z", kind: "html" },
			]),
			session("ao-2", "Review pricing", [{ name: "pricing.md", updatedAt: "2026-10-10T10:00:00Z" }]),
		],
	},
	{
		id: STANDALONE_WORKSPACE_ID,
		name: "Standalone",
		path: "",
		sessions: [
			session("standalone-1", "Compare caching options", [
				{ name: "caching-options.md", updatedAt: "2026-10-08T10:00:00Z" },
			]),
		],
	},
];

const withoutArtifacts: WorkspaceSummary[] = [
	{ id: "ao", name: "agent-orchestrator", path: "/code/ao", sessions: [] },
	{ id: "px0", name: "px0", path: "/code/px0", sessions: [] },
	{ id: "cloud", name: "cloud-project", path: "", kind: CLOUD_PROJECT_KIND, sessions: [] },
	{ id: STANDALONE_WORKSPACE_ID, name: "Standalone", path: "", sessions: [] },
];

function renderView() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<TooltipProvider>
				<ArtifactsView />
			</TooltipProvider>
		</QueryClientProvider>,
	);
}

describe("ArtifactsView", () => {
	afterEach(() => vi.unstubAllGlobals());
	beforeEach(() => {
		window.localStorage.removeItem("ao.artifacts.view");
		useArtifactsViewStore.setState({
			view: "gallery",
			collapsedSections: [],
			pinnedArtifactIds: [],
			pinAliases: {},
			galleryPreferences: {},
		});
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
				unobserve() {}
				disconnect() {}
			},
		);
		mocks.workspaces = [withArtifacts[1]];
		mocks.catalog.mockReset().mockResolvedValue({ items: [], errors: [], incomplete: false, revision: "r1" });
		mocks.agents = [codexReady];
		mocks.hideShellTopbar = true;
		mocks.navigate.mockReset();
		mocks.post.mockReset();
		mocks.get.mockReset();
		mocks.capture.mockReset();
		mocks.deleteCloudArtifact.mockReset().mockResolvedValue(undefined);
		useUiStore.setState({ settingsModal: null });
		useUiStore.getState().clearGlobalToast();
	});

	it("keeps gallery cloud actions on the cloud session instead of local file actions", async () => {
		const cloud = session("cloud-1", "Cloud notes", [{ name: "notes.md", updatedAt: "2026-10-11T10:00:00Z" }]);
		cloud.cloud = { orgId: "org-1" };
		mocks.workspaces = [
			{
				id: "cloud-project",
				name: "Cloud project",
				path: "",
				kind: CLOUD_PROJECT_KIND,
				cloudOrgId: "org-1",
				sessions: [cloud],
			},
		];
		renderView();
		await userEvent.click(screen.getByRole("button", { name: "More actions: notes.md" }));
		expect(screen.queryByRole("menuitem", { name: /^Show in / })).not.toBeInTheDocument();
		expect(screen.getByRole("menuitem", { name: "Download" })).toBeInTheDocument();
		await userEvent.click(screen.getByRole("menuitem", { name: "Delete artifact" }));
		const dialog = screen.getByRole("dialog");
		expect(dialog).toHaveTextContent("from the session's cloud storage and from its sandbox");
		await userEvent.click(within(dialog).getByRole("button", { name: "Delete artifact" }));
		await waitFor(() => expect(mocks.deleteCloudArtifact).toHaveBeenCalledWith("org-1", "cloud-1", "notes.md"));
	});

	it.each(["html", "markdown"] as const)("previews a cloud %s artifact using cloud access", async (kind) => {
		const name = kind === "html" ? "report.html" : "notes.md";
		const cloud = session("cloud-1", "Cloud notes", [{ name, kind, updatedAt: "2026-10-11T10:00:00Z" }]);
		cloud.cloud = { orgId: "org-1" };
		mocks.workspaces = [
			{
				id: "cloud-project",
				name: "Cloud project",
				path: "",
				kind: CLOUD_PROJECT_KIND,
				cloudOrgId: "org-1",
				sessions: [cloud],
			},
		];
		renderView();
		await userEvent.click(screen.getByRole("button", { name: new RegExp(`^${name}`) }));
		const dialog = screen.getByRole("dialog", { name });
		if (kind === "html") {
			expect(within(dialog).getByTestId("cloud-artifact-preview")).toHaveAttribute(
				"src",
				`https://cp.test/artifact-files/token/${name}?v=2026-10-11T10%3A00%3A00Z`,
			);
		} else {
			const preview = within(dialog).getByTestId("artifact-file-preview");
			expect(preview).toHaveAttribute("data-cloud", "true");
			expect(preview).toHaveAttribute("data-read-only", "true");
			expect(preview).toHaveAttribute(
				"data-raw-url",
				`https://cp.test/artifact-files/token/${name}?v=2026-10-11T10%3A00%3A00Z`,
			);
		}
	});

	it("keeps Scratchpad preview, pinning, deletion menu, and its source route", async () => {
		const user = userEvent.setup();
		renderView();
		const trigger = screen.getByRole("button", { name: /^caching-options\.md/ });
		await user.click(trigger);
		const modal = within(screen.getByRole("dialog", { name: "caching-options.md" }));
		expect(mocks.navigate).not.toHaveBeenCalled();
		await user.click(modal.getByRole("button", { name: "More actions: caching-options.md" }));
		expect(screen.getByRole("menuitem", { name: "Delete artifact" })).toBeVisible();
		await user.click(screen.getByRole("menuitem", { name: "Pin artifact" }));
		expect(useArtifactsViewStore.getState().pinnedArtifactIds.some((id) => id.startsWith('["artifact-v2",'))).toBe(
			true,
		);
		await user.click(modal.getByRole("button", { name: "Open in session" }));
		expect(mocks.navigate).toHaveBeenCalledWith({ to: "/sessions/$sessionId", params: { sessionId: "standalone-1" } });
	});
	it("keeps Scratchpad list preview and Escape focus restoration", async () => {
		const user = userEvent.setup();
		renderView();
		await user.click(screen.getByRole("button", { name: "List view" }));
		const trigger = screen.getByRole("button", { name: /^caching-options\.md/ });
		await user.click(trigger);
		expect(screen.getByRole("dialog", { name: "caching-options.md" })).toBeVisible();
		await user.keyboard("{Escape}");
		await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
		expect(trigger).toHaveFocus();
		expect(mocks.navigate).not.toHaveBeenCalled();
	});
	it("does not read or frame Cloud and Scratchpad thumbnails before opening", async () => {
		const fetch = vi.fn();
		vi.stubGlobal("fetch", fetch);
		const cloud = session("cloud-html", "Cloud report", [
			{ name: "report.html", kind: "html", updatedAt: "2026-10-11T10:00:00Z" },
		]);
		cloud.cloud = { orgId: "org-1" };
		mocks.workspaces = [
			...mocks.workspaces,
			{
				id: "cloud-project",
				name: "Cloud project",
				path: "",
				kind: CLOUD_PROJECT_KIND,
				cloudOrgId: "org-1",
				sessions: [cloud],
			},
		];
		const { container } = renderView();
		expect(container.querySelector("iframe, img, table")).toBeNull();
		expect(fetch).not.toHaveBeenCalled();
		expect(mocks.catalog).not.toHaveBeenCalled();
	});
	it("offers New artifact beside the filters with the same Scratchpad and project menu", async () => {
		const user = userEvent.setup();
		mocks.post.mockResolvedValue({ data: { session: { id: "standalone-7" } } });
		renderView();

		await user.click(screen.getByRole("button", { name: "New artifact" }));
		expect(screen.getAllByRole("menuitem").map((item) => item.textContent)).toEqual(["Scratchpad"]);
		await user.click(screen.getByRole("menuitem", { name: "Scratchpad" }));

		await waitFor(() =>
			expect(mocks.navigate).toHaveBeenCalledWith({
				to: "/sessions/$sessionId",
				params: { sessionId: "standalone-7" },
			}),
		);
		expect(mocks.post).toHaveBeenCalledWith("/api/v1/sessions", {
			body: { kind: "worker", requestedOutput: "artifact", displayName: "New artifact", harness: "codex" },
		});
	});

	it("disables New artifact when no agent is ready", () => {
		mocks.agents = [{ ...codexReady, installation: { state: "not_installed", freshness: "fresh" } }];
		renderView();

		expect(screen.getByRole("button", { name: "New artifact" })).toBeDisabled();
	});

	it("keeps filters and one Create artifact action when nothing exists", async () => {
		const user = userEvent.setup();
		mocks.workspaces = withoutArtifacts;
		renderView();

		expect(await screen.findByRole("heading", { name: "Create your first artifact" })).toBeInTheDocument();
		expect(screen.getByRole("searchbox", { name: "Search artifacts" })).toBeVisible();
		expect(screen.queryByRole("combobox")).not.toBeInTheDocument();

		await user.click(await screen.findByRole("button", { name: "Create artifact" }));
		expect(screen.getAllByRole("menuitem").map((item) => item.textContent)).toEqual([
			"Scratchpad",
			"agent-orchestrator",
			"px0",
		]);
	});

	it("starts a Scratchpad artifact session without a project id and opens it", async () => {
		const user = userEvent.setup();
		mocks.workspaces = withoutArtifacts;
		mocks.post.mockResolvedValue({ data: { session: { id: "standalone-9" } } });
		renderView();

		await user.click(await screen.findByRole("button", { name: "Create artifact" }));
		await user.click(screen.getByRole("menuitem", { name: "Scratchpad" }));

		await waitFor(() =>
			expect(mocks.navigate).toHaveBeenCalledWith({
				to: "/sessions/$sessionId",
				params: { sessionId: "standalone-9" },
			}),
		);
		expect(mocks.post).toHaveBeenCalledWith("/api/v1/sessions", {
			body: { kind: "worker", requestedOutput: "artifact", displayName: "New artifact", harness: "codex" },
		});
		expect(mocks.capture).not.toHaveBeenCalledWith("ao.renderer.artifact_create_requested", expect.anything());
		expect(mocks.capture).toHaveBeenCalledWith("ao.renderer.artifact_create_succeeded", { scope: "standalone" });
	});

	it("starts a project artifact session with the project's worker agent", async () => {
		const user = userEvent.setup();
		mocks.workspaces = withoutArtifacts;
		mocks.get.mockResolvedValue({
			data: { status: "ok", project: { agent: "codex", config: { worker: { agent: "claude-code" } } } },
		});
		mocks.post.mockResolvedValue({ data: { session: { id: "px0-3" } } });
		renderView();

		await user.click(await screen.findByRole("button", { name: "Create artifact" }));
		await user.click(screen.getByRole("menuitem", { name: "px0" }));

		await waitFor(() =>
			expect(mocks.navigate).toHaveBeenCalledWith({
				to: "/projects/$projectId/sessions/$sessionId",
				params: { projectId: "px0", sessionId: "px0-3" },
			}),
		);
		expect(mocks.post).toHaveBeenCalledWith("/api/v1/sessions", {
			body: {
				kind: "worker",
				requestedOutput: "artifact",
				displayName: "New artifact",
				projectId: "px0",
				harness: "claude-code",
			},
		});
		expect(mocks.capture).toHaveBeenCalledWith("ao.renderer.artifact_create_succeeded", { scope: "project" });
	});

	it("shows the daemon error and stays on the screen when the start fails", async () => {
		const user = userEvent.setup();
		mocks.workspaces = withoutArtifacts;
		mocks.post.mockResolvedValue({
			error: { error: "conflict", code: "AGENT_NOT_READY", message: "Agent is not ready" },
		});
		renderView();

		await user.click(await screen.findByRole("button", { name: "Create artifact" }));
		await user.click(screen.getByRole("menuitem", { name: "Scratchpad" }));

		await waitFor(() => expect(useUiStore.getState().globalToast).toMatchObject({ title: "Agent is not ready" }));
		expect(mocks.navigate).not.toHaveBeenCalled();
		expect(mocks.capture).toHaveBeenCalledWith("ao.renderer.artifact_create_failed", { scope: "standalone" });
		expect(await screen.findByRole("button", { name: "Create artifact" })).toBeEnabled();
	});

	it("points to agent settings when no agent is ready", async () => {
		const user = userEvent.setup();
		mocks.workspaces = withoutArtifacts;
		mocks.agents = [{ ...codexReady, installation: { state: "not_installed", freshness: "fresh" } }];
		renderView();

		expect(await screen.findByRole("heading", { name: "No agent is ready" })).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: "Create artifact" })).not.toBeInTheDocument();
		await user.click(screen.getByRole("button", { name: "Open agent settings" }));
		expect(useUiStore.getState().settingsModal).toMatchObject({ scope: "global", section: "agents" });
	});
});
