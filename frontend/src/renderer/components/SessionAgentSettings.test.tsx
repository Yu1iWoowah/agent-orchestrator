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
import { workspaceQueryKey } from "../hooks/useWorkspaceQuery";
import { terminateSessionMutationKey } from "../hooks/useTerminateSession";
import { appI18n } from "../i18n";

const { getMock, patchMock, postMock, captureRendererEventMock, connection } = vi.hoisted(() => ({ getMock: vi.fn(), patchMock: vi.fn(), postMock: vi.fn(), captureRendererEventMock: vi.fn(), connection: { trusted: true } }));
vi.mock("../lib/telemetry", async (importOriginal) => ({
	...await importOriginal<typeof import("../lib/telemetry")>(),
	captureRendererEvent: captureRendererEventMock,
}));
vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, PATCH: patchMock, POST: postMock },
	getApiBaseUrl: () => "http://127.0.0.1:43791",
	hasTrustedApiBaseUrl: () => connection.trusted,
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
	connection.trusted = true;
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
	captureRendererEventMock.mockClear();
});
afterEach(() => {
	cleanup();
	for (const client of clients.splice(0)) client.clear();
});

function showSession(overrides: Partial<WorkspaceSession> = {}, hostId?: string) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	clients.push(client);
	render(<QueryClientProvider client={client}><TooltipProvider><SessionAgentSettings hostId={hostId} session={{ ...session, ...overrides }} /></TooltipProvider></QueryClientProvider>);
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

function showSettingsSurfaces() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	clients.push(client);
	render(<QueryClientProvider client={client}><TooltipProvider>
		<AgentSettingsStatus enabled placement="header" sessionId="sess-1" />
		<AgentSettingsStatus enabled placement="composer" sessionId="sess-1" />
		<SessionAgentSettings session={session} />
	</TooltipProvider></QueryClientProvider>);
	return client;
}

describe("Session agent settings intent", () => {
	it("keeps accepted-to-failed results visible beside every action and reviews the original selection before a new retry", async () => {
		const client = showSettingsSurfaces();
		const editor = await editModel();
		await userEvent.click(within(editor).getByRole("button", { name: "Apply settings" }));
		const composer = screen.getByTestId("agent-settings-composer");
		await within(composer).findByRole("status");
		const accepted = server.pending;
		expect(accepted).toBeDefined();
		if (!accepted) throw new Error("The fixture must accept the first change");
		server = agentSettingsSnapshot({ revision: 6, settings: agentSettingsSelection({ effort: "high" }), pending: undefined, history: [{ ...accepted, phase: "failed", checkpoint: "restored", errorCode: "AGENT_SETTINGS_FAILED", errorCategory: "provider", requestId: "failed-request" }] });
		act(() => { client.setQueryData(agentSettingsQueryKey("sess-1"), server); });
		for (const surface of [screen.getByTestId("agent-settings-header"), composer, screen.getByRole("region", { name: "Agent settings" })]) {
			const alert = await within(surface).findByRole("alert");
			expect(alert).toHaveTextContent("AGENT_SETTINGS_FAILED");
			expect(alert).toHaveTextContent("failed-request");
			expect(alert).toHaveTextContent("The provider could not apply the requested settings.");
		}
		expect(screen.getByText("Recent settings changes").closest("details")).not.toHaveAttribute("open");
		expect(within(composer).queryByRole("status")).not.toBeInTheDocument();
		expect(patchMock).toHaveBeenCalledTimes(1);
		await userEvent.click(within(composer).getByRole("button", { name: "Review and retry" }));
		const retry = screen.getByRole("dialog", { name: "Change agent settings" });
		expect(within(retry).getByRole("button", { name: /^Model/ })).toHaveTextContent("pending-model");
		expect(within(retry).getByRole("button", { name: /^Effort/ })).toHaveTextContent("high");
		expect(patchMock).toHaveBeenCalledTimes(1);
		await userEvent.click(within(retry).getByRole("button", { name: "Apply settings" }));
		await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(2));
		expect(patchMock.mock.calls[1][1].body.expectedRevision).toBe(6);
		expect(patchMock.mock.calls[1][1].body.settings).toEqual({ model: "pending-model" });
		expect(patchMock.mock.calls[1][1].headers["Idempotency-Key"]).not.toBe(patchMock.mock.calls[0][1].headers["Idempotency-Key"]);
	});

	it("dismisses a failed notice across placements without mutating history, and shows a later failure", async () => {
		const failure = agentSettingsOperation({ phase: "failed", errorCode: "AGENT_SETTINGS_FAILED" });
		server = agentSettingsSnapshot({ pending: undefined, history: [failure] });
		const client = showSettingsSurfaces();
		const header = await screen.findByTestId("agent-settings-header");
		await within(header).findByRole("alert");
		await userEvent.click(within(header).getByRole("button", { name: "Keep current settings" }));
		expect(screen.queryAllByRole("alert")).toHaveLength(0);
		expect(server.history).toEqual([failure]);
		expect(patchMock).not.toHaveBeenCalled();
		expect(postMock).not.toHaveBeenCalled();
		act(() => { client.setQueryData(agentSettingsQueryKey("sess-1"), { ...server, revision: 7 }); });
		expect(screen.queryAllByRole("alert")).toHaveLength(0);
		server = { ...server, history: [{ ...failure, id: "later-failure", requestId: "later-request" }, failure] };
		act(() => { client.setQueryData(agentSettingsQueryKey("sess-1"), server); });
		expect(await within(header).findByRole("alert")).toHaveTextContent("later-request");
	});

	it("preserves a retry draft when another controller completes an operation", async () => {
		server = agentSettingsSnapshot({ pending: undefined, history: [agentSettingsOperation({ phase: "failed", errorCategory: "provider" })] });
		const client = showSettingsSurfaces();
		const composer = await screen.findByTestId("agent-settings-composer");
		await userEvent.click(await within(composer).findByRole("button", { name: "Review and retry" }));
		const dialog = screen.getByRole("dialog", { name: "Change agent settings" });
		server = agentSettingsSnapshot({ revision: 7, pending: undefined, settings: agentSettingsSelection({ model: "external-model" }), history: [agentSettingsOperation({ id: "newer-applied", phase: "applied" }), ...server.history ?? []] });
		act(() => { client.setQueryData(agentSettingsQueryKey("sess-1"), server); });
		await within(dialog).findByText("Settings changed elsewhere. Your draft is preserved; applying it still checks the revision you began editing.");
		expect(within(dialog).getByRole("button", { name: /^Model/ })).toHaveTextContent("pending-model");
		expect(screen.queryAllByRole("alert")).toHaveLength(0);
		expect(patchMock).not.toHaveBeenCalled();
		await userEvent.keyboard("{Escape}");
	});

	it("keeps failure dismissal scoped to its host even with equal session IDs", async () => {
		server = agentSettingsSnapshot({ pending: undefined, history: [agentSettingsOperation({ phase: "failed" })] });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		clients.push(client);
		render(<QueryClientProvider client={client}><AgentSettingsStatus enabled hostId="host-a" placement="header" sessionId="sess-1" /><AgentSettingsStatus enabled hostId="host-b" placement="composer" sessionId="sess-1" /></QueryClientProvider>);
		const header = await screen.findByTestId("agent-settings-header");
		const composer = await screen.findByTestId("agent-settings-composer");
		await within(composer).findByRole("alert");
		await userEvent.click(within(header).getByRole("button", { name: "Keep current settings" }));
		expect(within(header).queryByRole("alert")).not.toBeInTheDocument();
		expect(within(composer).getByRole("alert")).toBeVisible();
	});

	it("requires fresh stop-turn consent when reviewing a failed TUI intent", async () => {
		server = agentSettingsSnapshot({ revision: 8, restartRequired: true, pending: undefined, history: [agentSettingsOperation({ phase: "failed", applyMode: "restart", stopTurn: true })] });
		showSession({ mode: "tui" });
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		await userEvent.click(await screen.findByRole("button", { name: "Review and retry" }));
		const editor = screen.getByRole("dialog", { name: "Change agent settings" });
		expect(within(editor).getByRole("button", { name: /^Model/ })).toHaveTextContent("pending-model");
		await userEvent.click(within(editor).getByRole("button", { name: "Review restart" }));
		const confirmation = screen.getByRole("dialog", { name: "Restart this session" });
		expect(within(confirmation).getByRole("checkbox", { name: "Allow stopping the current turn" })).not.toBeChecked();
		await userEvent.click(within(confirmation).getByRole("button", { name: "Confirm restart" }));
		await waitFor(() => expect(patchMock).toHaveBeenCalledTimes(1));
		expect(patchMock.mock.calls[0][1].body).toMatchObject({ expectedRevision: 8, applyMode: "restart", stopTurn: false, settings: { model: "pending-model", effort: "high", fastMode: "on" } });
	});
	it("shows the failed reason and next actions in Traditional Chinese", async () => {
		await act(async () => { await appI18n.changeLanguage("zh-TW"); });
		try {
			server = agentSettingsSnapshot({ pending: undefined, history: [agentSettingsOperation({ phase: "failed", errorCode: "AGENT_SETTINGS_FAILED", errorCategory: "provider" })] });
			showSession();
			await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
			const alert = await screen.findByRole("alert");
			expect(alert).toHaveTextContent("設定變更失敗");
			expect(alert).toHaveTextContent("Provider 無法套用要求的設定。");
			expect(within(alert).getByRole("button", { name: "檢視並重試" })).toBeVisible();
			expect(within(alert).getByRole("button", { name: "保留目前設定" })).toBeVisible();
		} finally {
			await act(async () => { await appI18n.changeLanguage("en"); });
		}
	});
	it("does not bypass local daemon readiness through the local host alias", async () => {
		connection.trusted = false;
		showSession({}, "local");
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		expect(getMock).not.toHaveBeenCalled();
	});
	it("exposes a provider observation reason in a disclosure without guessing an effective value", async () => {
		showSession();
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		const summary = await screen.findByText("Fast observation details");
		expect(screen.getByText("provider_not_provided")).not.toBeVisible();
		await userEvent.click(summary);
		expect(screen.getByText("provider_not_provided")).toBeVisible();
		expect(within(screen.getByRole("group", { name: "Provider effective values" })).getByText("Not provided")).toBeVisible();
	});
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

	it.each([
		{ language: "en", label: "Archive session", title: "Are you sure you want to archive this session?", description: /clean worktree will be removed.*branch kept.*unconfirmed writer.*workspace and launch veto/i },
		{ language: "zh-TW", label: "封存 session", title: "確定要封存這個 session 嗎？", description: /乾淨的 worktree 會被移除.*branch 會保留.*writer.*workspace.*啟動限制/ },
	])("explains recovery archive consequences and uses shared archive state ($language)", async ({ language, label, title, description }) => {
		await act(async () => { await appI18n.changeLanguage(language); });
		try {
			server = agentSettingsSnapshot({ restartRequired: true, pending: agentSettingsOperation({ phase: "recovery_required", applyMode: "restart", startedAt: "2026-10-30T09:00:01Z" }) });
			postMock.mockResolvedValue({ data: { ok: true } });
			const client = showSession({ mode: "tui" });
			client.setQueryData(workspaceQueryKey, [{ id: "ws-1", kind: "single_repo", name: "Fake workspace", path: "/fake/workspace", sessions: [{ ...session, mode: "tui" }] }]);
			await userEvent.click(screen.getByRole("button", { name: appI18n.t("agentSettings.title") }));
			await userEvent.click(await screen.findByRole("button", { name: label }));
			const dialog = await screen.findByRole("dialog", { name: title });
			expect(dialog).toHaveTextContent(description);
			expect(postMock).not.toHaveBeenCalled();
			await userEvent.click(within(dialog).getByRole("button", { name: label }));
			await waitFor(() => expect(client.getMutationCache().find({ mutationKey: terminateSessionMutationKey })?.state.status).toBe("success"));
			expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/kill", expect.objectContaining({ params: { path: { sessionId: "sess-1" } }, body: undefined }));
			expect(client.getQueryData(workspaceQueryKey)).toEqual([expect.objectContaining({ sessions: [expect.objectContaining({ id: "sess-1", isTerminated: true })] })]);
			expect(captureRendererEventMock).toHaveBeenCalledWith("ao.renderer.session_kill_succeeded", { project_id: "ws-1" });
		} finally {
			await act(async () => { await appI18n.changeLanguage("en"); });
		}
	});

	it("restores shared session state and offers an explicit retry when recovery archive fails", async () => {
		server = agentSettingsSnapshot({ restartRequired: true, pending: agentSettingsOperation({ phase: "recovery_required", applyMode: "restart", startedAt: "2026-10-30T09:00:01Z" }) });
		postMock.mockResolvedValueOnce({ error: { message: "Writer is still unconfirmed" }, response: new Response(null, { status: 409 }) }).mockResolvedValue({ data: { ok: true } });
		const client = showSession({ mode: "tui" });
		const workspaces = [{ id: "ws-1", kind: "single_repo", name: "Fake workspace", path: "/fake/workspace", sessions: [{ ...session, mode: "tui" }] }];
		client.setQueryData(workspaceQueryKey, workspaces);
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		await userEvent.click(await screen.findByRole("button", { name: "Archive session" }));
		const dialog = screen.getByRole("dialog", { name: "Are you sure you want to archive this session?" });
		await userEvent.click(within(dialog).getByRole("button", { name: "Archive session" }));
		expect(await within(dialog).findByRole("alert")).toHaveTextContent("Writer is still unconfirmed");
		expect(client.getQueryData(workspaceQueryKey)).toEqual(workspaces);
		expect(captureRendererEventMock).toHaveBeenCalledWith("ao.renderer.session_kill_failed", { project_id: "ws-1" });
		await userEvent.click(within(dialog).getByRole("button", { name: "Archive session" }));
		await waitFor(() => expect(client.getMutationCache().findAll({ mutationKey: terminateSessionMutationKey }).at(-1)?.state.status).toBe("success"));
		expect(postMock).toHaveBeenCalledTimes(2);
	});

	it.each([
		{ mode: "chat", applyMode: "after-turn", isTerminated: false },
		{ mode: "chat", applyMode: "restart", isTerminated: false },
		{ mode: "tui", applyMode: "after-turn", isTerminated: false },
		{ mode: "tui", applyMode: "restart", isTerminated: true },
	] satisfies { mode: "chat" | "tui"; applyMode: "after-turn" | "restart"; isTerminated: boolean }[])("does not offer recovery archive outside a live TUI restart ($mode, $applyMode, terminated=$isTerminated)", async ({ mode, applyMode, isTerminated }) => {
		server = agentSettingsSnapshot({ pending: agentSettingsOperation({ phase: "recovery_required", applyMode, startedAt: "2026-10-30T09:00:01Z" }) });
		showSession({ mode, isTerminated });
		await userEvent.click(screen.getByRole("button", { name: "Agent settings" }));
		await screen.findByText("This operation has started. Further choices may return a conflict until it finishes.");
		expect(screen.queryByRole("button", { name: /Archive session|Retry stopping/ })).not.toBeInTheDocument();
		expect(postMock).not.toHaveBeenCalled();
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
