import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { components } from "../../api/schema";
import { agentSettingsQueryKey, type AgentSettingsSnapshot } from "../lib/agent-settings";
import { agentSettingsOperation, agentSettingsSelection, agentSettingsSnapshot } from "../test/agent-settings-fixtures";
import type { WorkspaceSession } from "../types/workspace";
import { SessionAgentSettings } from "./SessionAgentSettings";
import { AgentSettingsStatus } from "./AgentSettingsStatus";
import { TooltipProvider } from "./ui/tooltip";

const { getMock, patchMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), patchMock: vi.fn(), postMock: vi.fn() }));
vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, PATCH: patchMock, POST: postMock },
	getApiBaseUrl: () => "http://127.0.0.1:43791",
	hasTrustedApiBaseUrl: () => true,
	subscribeApiBaseUrl: () => () => {},
	apiErrorMessage: (error: unknown, fallback = "Request failed") => error instanceof Error ? error.message
		: typeof error === "object" && error !== null && "message" in error && typeof error.message === "string" ? error.message : fallback,
	apiErrorCode: (error: unknown) => typeof error === "object" && error !== null && "code" in error && typeof error.code === "string" ? error.code : undefined,
	apiErrorRequestId: (error: unknown) => typeof error === "object" && error !== null && "requestId" in error && typeof error.requestId === "string" ? error.requestId : undefined,
}));
vi.mock("../lib/host-clients", async (importOriginal) => ({
	...await importOriginal<typeof import("../lib/host-clients")>(),
	clientForSessionHost: () => ({ GET: getMock, PATCH: patchMock, POST: postMock }),
}));

let server: AgentSettingsSnapshot;
const clients: QueryClient[] = [];
const session: WorkspaceSession = { id: "sess-1", workspaceId: "ws-1", workspaceName: "Fake workspace", title: "Fake task", provider: "codex", kind: "worker", mode: "chat", status: "idle", updatedAt: "2026-10-30T09:00:00Z", prs: [] };

beforeEach(() => {
	server = agentSettingsSnapshot({ pending: undefined });
	getMock.mockReset().mockImplementation(async (path: string) => path === "/api/v1/sessions/{sessionId}/agent-settings"
		? { data: server }
		: { error: { code: "NOT_IMPLEMENTED", message: "Unavailable in fake fixture" } });
	patchMock.mockReset().mockImplementation(async (_path: string, options: { body: components["schemas"]["ChangeSessionAgentSettingsPayload"] }) => {
		const patch = options.body.settings;
		const target = {
			model: patch.model === undefined ? server.settings.model : patch.model ?? "",
			effort: patch.effort === undefined ? server.settings.effort : patch.effort ?? "",
			fastMode: patch.fastMode ?? server.settings.fastMode,
		};
		const operation = agentSettingsOperation({ target, applyMode: options.body.applyMode, stopTurn: options.body.stopTurn });
		server = { ...server, pending: operation };
		return { data: { operation }, response: new Response(null, { status: 202 }) };
	});
	postMock.mockReset();
});
afterEach(() => {
	cleanup();
	for (const client of clients.splice(0)) client.clear();
});

function showSession(overrides: Partial<WorkspaceSession> = {}) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	clients.push(client);
	render(<QueryClientProvider client={client}><TooltipProvider><SessionAgentSettings session={{ ...session, ...overrides }} /></TooltipProvider></QueryClientProvider>);
	return client;
}

async function editModel() {
	await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
	await userEvent.click(await screen.findByRole("button", { name: "Edit agent settings" }));
	const dialog = await screen.findByRole("dialog", { name: "Change agent settings" });
	await userEvent.click(within(dialog).getByRole("button", { name: /^Model/ }));
	await userEvent.click(await screen.findByRole("menuitem", { name: /pending-model/i }));
	return dialog;
}

describe("Session agent settings intent", () => {
	it("reports malformed or wrong-session readback instead of displaying another selection", async () => {
		getMock.mockResolvedValue({ data: { sessionId: "different-session", revision: 4 } });
		showSession();
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		expect(await screen.findByRole("alert")).toHaveTextContent("Agent settings could not be loaded");
		expect(screen.queryByRole("button", { name: "Edit agent settings" })).not.toBeInTheDocument();
	});

	it("keeps unknown capabilities usable when an empty catalog is encoded as null", async () => {
		getMock.mockResolvedValue({ data: { ...server, capabilities: { ...server.capabilities, state: "unknown", reason: "catalog_stale", models: null } } });
		showSession();
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		await userEvent.click(await screen.findByRole("button", { name: "Edit agent settings" }));
		expect(screen.getByRole("dialog", { name: "Change agent settings" })).toHaveTextContent("catalog_stale");
	});
	it("reads the public account label and labels spawn provenance independently of effective fast", async () => {
		server = agentSettingsSnapshot({ pending: undefined });
		server.capabilities.accountId = "account-A";
		getMock.mockImplementation(async (path: string) => path === "/api/v1/agents/accounts"
			? { data: { accounts: [{ id: "account-A", provider: "codex", email: "isolated@example.invalid", default: true, device: false, signedIn: true }] } }
			: { data: server });
		showSession({ spawnAgentSettings: { settings: agentSettingsSelection(), sources: { model: "role", effort: "project", fastMode: "explicit" }, validation: "catalog", effortCleared: false } });
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		expect(await screen.findByText("isolated@example.invalid")).toBeVisible();
		await userEvent.click(screen.getByText("Spawn selection sources"));
		expect(screen.getByText("role", { exact: true })).toBeVisible();
		expect(screen.getByText("project", { exact: true })).toBeVisible();
		expect(screen.getByText("explicit", { exact: true })).toBeVisible();
		expect(within(screen.getByRole("group", { name: "Provider effective values" })).getByText("Not provided")).toBeVisible();
	});
	it("keeps committed header separate from pending, and attributes only server-verified orchestrator history", async () => {
		server = agentSettingsSnapshot({ history: [
			{ ...agentSettingsOperation({ id: "verified", actor: { kind: "orchestrator", sessionId: "trusted-orchestrator" }, phase: "applied" }) },
			{ ...agentSettingsOperation({ id: "unverified", actor: { kind: "local_operator_unverified", sessionId: "claimed-orchestrator" } }) },
		] });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		clients.push(client);
		render(<QueryClientProvider client={client}><AgentSettingsStatus enabled placement="header" sessionId="sess-1" /><AgentSettingsStatus enabled placement="composer" sessionId="sess-1" /><AgentSettingsStatus enabled placement="timeline" sessionId="sess-1" /></QueryClientProvider>);
		const header = await screen.findByTestId("agent-settings-header");
		expect(within(header).getByText(/^Current selection:/)).toHaveTextContent("committed-model");
		expect(within(header).getByRole("status")).toHaveTextContent("pending-model");
		expect(within(header).getByText(/Not provided/)).toBeVisible();
		expect(screen.getByTestId("agent-settings-composer")).toHaveTextContent("pending-model");
		const activities = screen.getByTestId("agent-settings-activities");
		expect(activities).toHaveTextContent("By orchestrator trusted-orchestrator");
		expect(activities).not.toHaveTextContent("claimed-orchestrator");
		expect(activities).toHaveTextContent("applied");
	});
	it("cancels only an unstarted pending operation using its captured version", async () => {
		server = agentSettingsSnapshot();
		postMock.mockResolvedValue({ data: { operation: agentSettingsOperation({ phase: "cancelled" }) } });
		showSession();
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		await userEvent.click(await screen.findByRole("button", { name: "Cancel pending settings" }));
		await waitFor(() => expect(postMock).toHaveBeenCalledTimes(1));
		expect(postMock.mock.calls[0][0]).toContain("/cancel");
		expect(postMock.mock.calls[0][1].body).toEqual({ expectedVersion: 1 });
	});

	it("keeps an unconfirmed writer fenced after a failed operator check, with explicit stop consent", async () => {
		server = agentSettingsSnapshot({ pending: undefined, writerRecovery: { operationId: "settings-op-1", version: 7, state: "writer_unconfirmed" } });
		postMock.mockResolvedValue({ error: { code: "AGENT_SETTINGS_WRITER_UNCONFIRMED", message: "Writer is still unconfirmed", requestId: "writer-request" } });
		showSession({ mode: "tui", isTerminated: true });
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		await userEvent.click(await screen.findByRole("button", { name: "Check retained writer" }));
		const dialog = await screen.findByRole("dialog", { name: "Check native writer" });
		const consent = within(dialog).getByRole("checkbox", { name: "Allow stopping an identified writer" });
		expect(consent).not.toBeChecked();
		expect(postMock).not.toHaveBeenCalled();
		await userEvent.click(consent);
		await userEvent.click(within(dialog).getByRole("button", { name: "Check writer" }));
		expect(await within(dialog).findByText(/writer-request/)).toBeVisible();
		expect(postMock.mock.calls[0][1].body).toEqual({ expectedVersion: 7, stopIfAlive: true });
		expect(screen.getByText(/workspace is retained/)).toBeVisible();
	});

	it("never offers cancellation after a pending operation starts", async () => {
		server = agentSettingsSnapshot({ pending: agentSettingsOperation({ checkpoint: "applying_model", startedAt: "2026-10-30T09:00:01Z" }) });
		showSession();
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		await screen.findByRole("group", { name: "Pending for next turn" });
		expect(screen.queryByRole("button", { name: "Cancel pending settings" })).not.toBeInTheDocument();
	});
	it("keeps an unsent draft across external settings updates and restores focus on Escape", async () => {
		const client = showSession();
		const dialog = await editModel();
		server = agentSettingsSnapshot({ revision: 5, settings: agentSettingsSelection({ model: "external-model" }), pending: undefined });
		act(() => { client.setQueryData(agentSettingsQueryKey("sess-1"), server); });
		expect(within(dialog).getByRole("button", { name: /^Model/ })).toHaveTextContent("pending-model");
		expect(patchMock).not.toHaveBeenCalled();
		await userEvent.keyboard("{Escape}");
		await waitFor(() => expect(screen.getByRole("button", { name: "Edit agent settings" })).toHaveFocus());
	});

	it("keeps the draft and request ID after revision conflict without an automatic overwrite", async () => {
		showSession();
		const dialog = await editModel();
		patchMock.mockResolvedValueOnce({ error: { code: "AGENT_SETTINGS_CONFLICT", message: "Settings changed elsewhere", requestId: "cas-request" }, response: new Response(null, { status: 409 }) });
		await userEvent.click(within(dialog).getByRole("button", { name: "Apply settings" }));
		expect(await within(dialog).findByText(/cas-request/)).toBeVisible();
		expect(within(dialog).getByRole("button", { name: /^Model/ })).toHaveTextContent("pending-model");
		expect(patchMock).toHaveBeenCalledTimes(1);
		expect(patchMock.mock.calls[0][1].body).toMatchObject({ expectedRevision: 4, settings: { model: "pending-model" } });
	});

	it("adopts a newer revision only on explicit review, retaining untouched external fields", async () => {
		const client = showSession();
		const dialog = await editModel();
		server = agentSettingsSnapshot({ revision: 6, settings: agentSettingsSelection({ model: "external-model", effort: "high" }), pending: undefined });
		act(() => { client.setQueryData(agentSettingsQueryKey("sess-1"), server); });
		await userEvent.click(await within(dialog).findByRole("button", { name: "Use latest revision, keep my changes" }));
		expect(within(dialog).getByRole("button", { name: /^Model/ })).toHaveTextContent("pending-model");
		expect(within(dialog).getByRole("button", { name: /^Effort/ })).toHaveTextContent("high");
		await userEvent.click(within(dialog).getByRole("button", { name: "Apply settings" }));
		await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(1));
		expect(patchMock.mock.calls[0][1].body).toMatchObject({ expectedRevision: 6, settings: { model: "pending-model" } });
	});

	it.each([false, true])("requires TUI restart consent and explicit stop-turn=%s", async (stopTurn) => {
		server = agentSettingsSnapshot({ restartRequired: true, pending: undefined });
		showSession({ mode: "tui" });
		const dialog = await editModel();
		await userEvent.click(within(dialog).getByRole("button", { name: "Review restart" }));
		const confirmation = await screen.findByRole("dialog", { name: "Restart this session" });
		const stop = within(confirmation).getByRole("checkbox", { name: "Allow stopping the current turn" });
		expect(stop).not.toBeChecked();
		expect(patchMock).not.toHaveBeenCalled();
		if (stopTurn) await userEvent.click(stop);
		await userEvent.click(within(confirmation).getByRole("button", { name: "Confirm restart" }));
		await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(1));
		expect(patchMock.mock.calls[0][1].body).toMatchObject({ applyMode: "restart", stopTurn });
		const panel = screen.getByRole("region", { name: "Agent settings" });
		expect(within(within(panel).getByRole("group", { name: "Current selection" })).getByText("committed-model")).toBeVisible();
	});

	it("reuses the exact accepted intent after a disconnect instead of creating another operation", async () => {
		showSession();
		const dialog = await editModel();
		patchMock.mockRejectedValueOnce(new TypeError("Failed to fetch"));
		await userEvent.click(within(dialog).getByRole("button", { name: "Apply settings" }));
		await userEvent.click(await within(dialog).findByRole("button", { name: "Retry" }));
		await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(2));
		expect(patchMock.mock.calls[1][1].headers["Idempotency-Key"]).toBe(patchMock.mock.calls[0][1].headers["Idempotency-Key"]);
		expect(patchMock.mock.calls[1][1].body).toEqual(patchMock.mock.calls[0][1].body);
	});

	it("creates a new intent when restart stop-turn consent changes after a disconnect", async () => {
		server = agentSettingsSnapshot({ restartRequired: true, pending: undefined });
		showSession({ mode: "tui" });
		const editor = await editModel();
		await userEvent.click(within(editor).getByRole("button", { name: "Review restart" }));
		const dialog = screen.getByRole("dialog", { name: "Restart this session" });
		const consent = within(dialog).getByRole("checkbox", { name: "Allow stopping the current turn" });
		await userEvent.click(consent);
		patchMock.mockRejectedValueOnce(new TypeError("Failed to fetch"));
		await userEvent.click(within(dialog).getByRole("button", { name: "Confirm restart" }));
		await within(dialog).findByRole("alert");
		await userEvent.click(consent);
		await userEvent.click(within(dialog).getByRole("button", { name: /Confirm restart|Retry/ }));
		await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(2));
		expect(patchMock.mock.calls[1][1].body.stopTurn).toBe(false);
		expect(patchMock.mock.calls[1][1].headers["Idempotency-Key"]).not.toBe(patchMock.mock.calls[0][1].headers["Idempotency-Key"]);
	});
});
