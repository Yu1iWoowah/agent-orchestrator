import { createContext, useContext, type ComponentProps } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider, useParams } from "@tanstack/react-router";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ProjectQuestionRouting } from "./ProjectQuestionRouting";
import { ProjectAttention } from "./ProjectAttention";
import { SessionConversationFrames } from "./chat/SessionConversationFrames";
import { TooltipProvider } from "./ui/tooltip";
import { QuestionAnswerRuntime } from "../hooks/useQuestionAnswerRecovery";
import { questionSourceQueryOptions } from "../hooks/useSessionQuestions";
import { useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import { useTerminateSession } from "../hooks/useTerminateSession";
import { parseQuestionRef, type QuestionRef } from "../lib/decision-target";
import { sessionQuestionFixture } from "../test/fixtures/session-question";
import { useQuestionDraftStore } from "../stores/question-draft-store";
import { useConversationFrameStore } from "../stores/conversation-frame-store";
import type { WorkspaceSession, WorkspaceSummary } from "../types/workspace";

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }));
vi.mock("../lib/api-client", async (importOriginal) => ({
	...await importOriginal<typeof import("../lib/api-client")>(),
	apiClient: { GET: (path: string, request: unknown) => get(undefined, path, request), POST: (path: string, request: unknown) => post(undefined, path, request) }, hasTrustedApiBaseUrl: () => true,
}));
vi.mock("../hooks/useCloudCp", () => ({ useCloudCp: () => ({ ready: false, client: {}, baseUrl: "" }) }));
vi.mock("../hooks/useCloudOrg", () => ({ useCloudOrg: () => ({ ready: false }) }));
vi.mock("../lib/host-clients", () => ({ clientForSessionHost: (host?: string) => ({ GET: (path: string, request: unknown) => get(host, path, request), POST: (path: string, request: unknown) => post(host, path, request) }), labelForHost: (host: string) => host }));
let surface: HTMLElement;
let frameRect = { left: 16, top: 48, width: 900, height: 700 };
vi.mock("../hooks/useConversationFrameSafeRect", () => ({ useConversationFrameSafeRect: () => ({ surface, rect: frameRect }) }));
type Snapshot = Omit<ComponentProps<typeof ProjectQuestionRouting>, "children">;
const SnapshotContext = createContext<Snapshot | undefined>(undefined);
let client: QueryClient;
let questions: Map<string, ReturnType<typeof sessionQuestionFixture>>;
let providerRow = false;
const ref: QuestionRef = { hostId: "local", projectId: "project", originSessionId: "worker", questionId: "ask-worker" };
const questionURL = (path: string, value = ref) => `${path}?question=${encodeURIComponent(JSON.stringify(value))}`;
function session(id: string, kind: "orchestrator" | "worker" = "orchestrator", isTerminated = false): WorkspaceSession {
	return { id, workspaceId: "project", workspaceName: "Project", kind, isTerminated, mode: "chat", provider: "claude-code", status: isTerminated ? "terminated" : "idle", title: id, prs: [], createdAt: id === "main" ? "2026-10-01" : "2026-10-31", updatedAt: "2026-10-31" };
}
function workspace(sessions: WorkspaceSession[], hostId?: string): WorkspaceSummary {
	return { id: "project", name: "Project", path: "/fixture", sessions, hostId };
}
function Page() {
	const { sessionId, projectId, hostId } = useParams({ strict: false });
	return <><h1>{sessionId ? `Session ${sessionId}` : `Project ${projectId}`}</h1>
		{sessionId ? <SessionConversationFrames sessionId={sessionId} projectId={projectId} hostId={hostId} isOrchestrator={sessionId !== "worker"} activeBranchId="root" surfaceRef={{ current: surface }} composerRef={{ current: null }} /> : <ProjectAttention projectId={projectId ?? ""} hostId={hostId} />}</>;
}
let killMain: (() => void) | undefined;
function WorkspaceRoot() {
	const query = useWorkspaceQuery({ includeCloud: false });
	const terminate = useTerminateSession();
	const main = query.data?.flatMap((entry) => entry.sessions).find((entry) => entry.id === "main");
	killMain = main ? () => terminate.mutate(main) : undefined;
	return <ProjectQuestionRouting workspaces={query.data ?? []} localReady={query.isSuccess && !query.isError} remoteWorkspaces={[]}
		loadedProjectHostIds={[]} loadedSessionHostIds={[]} failedHostIds={[]} onRetry={() => { void query.refetch(); }}>
		<output data-testid="workspace-query">{query.status}:{query.fetchStatus}</output><Outlet />
	</ProjectQuestionRouting>;
}
function show(initialEntry: string, overrides: Partial<Snapshot> = {}, fromQuery = false) {
	let snapshot: Snapshot = { workspaces: [workspace([session("main"), session("worker", "worker")])], remoteWorkspaces: [], localReady: true, loadedProjectHostIds: [], loadedSessionHostIds: [], failedHostIds: [], onRetry: vi.fn(), ...overrides };
	function Root() {
		const facts = useContext(SnapshotContext);
		if (!facts) throw new Error("missing fixture facts");
		return <ProjectQuestionRouting {...facts}><Outlet /></ProjectQuestionRouting>;
	}
	const root = createRootRoute({ validateSearch: (search: Record<string, unknown>) => ({ question: parseQuestionRef(search.question), followOrchestrator: search.followOrchestrator === true ? true : undefined }), component: fromQuery ? WorkspaceRoot : Root });
	const routeTree = root.addChildren([
		createRoute({ getParentRoute: () => root, path: "/projects/$projectId", component: Page }),
		createRoute({ getParentRoute: () => root, path: "/projects/$projectId/sessions/$sessionId", component: Page }),
		createRoute({ getParentRoute: () => root, path: "/host/$hostId/project/$projectId", component: Page }),
		createRoute({ getParentRoute: () => root, path: "/host/$hostId/project/$projectId/session/$sessionId", component: Page }),
	]);
	const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: [initialEntry] }), defaultPendingMinMs: 0 });
	const subject = () => <QueryClientProvider client={client}><QuestionAnswerRuntime /><SnapshotContext.Provider value={snapshot}><TooltipProvider><RouterProvider router={router} /></TooltipProvider></SnapshotContext.Provider></QueryClientProvider>;
	const view = render(subject());
	return { router, snapshot, update: (patch: Partial<Snapshot>) => { snapshot = { ...snapshot, ...patch }; view.rerender(subject()); } };
}
beforeEach(() => {
	localStorage.clear(); useQuestionDraftStore.setState({ entries: {}, storageAvailable: true }); useConversationFrameStore.setState({ sessions: {} });
	frameRect = { left: 16, top: 48, width: 900, height: 700 };
	client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	surface = document.createElement("main"); surface.className = "center-panel-surface";
	surface.getBoundingClientRect = () => ({ x: 0, y: 0, left: 0, top: 0, right: 1000, bottom: 800, width: 1000, height: 800, toJSON: () => ({}) }); document.body.append(surface);
	questions = new Map(["main", "worker", "direct"].map((id) => {
		const q = sessionQuestionFixture({ id: `ask-${id}`, originSessionId: id, sourceReportId: id === "worker" ? "report-worker" : undefined });
		return [q.id, { ...q, payload: { ...q.payload, title: `${id} decision` } }];
	}));
	providerRow = false; get.mockReset(); post.mockReset();
	get.mockImplementation((_host, path, request) => {
		if (path.includes("/projects/")) {
			const rows = [...questions.values()].filter((q) => q.state === "open" && q.projectId === request.params.path.id).map((q) => ({ id: `question:${q.id}`, kind: "agent_question", sessionId: q.originSessionId, sessionName: q.originSessionId, questionId: q.id, title: q.payload.title, revision: q.version, read: q.read, createdAt: q.createdAt }));
			const extra = providerRow ? [{ id: "provider:p", kind: "provider_approval", sessionId: "worker", sessionName: "worker", title: "Worker provider request", revision: 1, read: false, createdAt: "2026-10-29T00:00:00Z", activityId: "p", conversationId: "c-worker", branchId: "worker-root", sourceBranchId: "worker-root", requestId: "rpc-worker" }] : [];
			return Promise.resolve({ data: { items: request.params.query.cursor ? rows.slice(2) : [...rows.slice(0, 2), ...extra], open: rows.length + extra.length, unread: rows.filter((q) => !q.read).length + extra.length, nextCursor: !request.params.query.cursor && rows.length > 2 ? "next" : undefined }, response: { status: 200 } });
		}
		if (path.endsWith("/plan")) return Promise.resolve({ data: { state: "absent", conversationId: "c", activeBranchId: "root" }, response: { status: 200 } });
		if (path.endsWith("/attention/{activityId}")) return Promise.resolve({ data: { conversationId: "c-worker", activeBranchId: "worker-root", items: [{ branchId: "worker-root", actionable: true, read: true, activity: { kind: "activity", id: "p", requestId: "rpc-worker", activityKind: "approval", status: "pending", sequence: 1, revision: 1, summary: "Provider command", createdAt: "2026-10-29T00:00:00Z" } }], counts: { open: 1, unread: 0, history: 0 }, hasMoreBefore: false }, response: { status: 200 } });
		if (path.endsWith("/attention")) return Promise.resolve({ data: { conversationId: "c", activeBranchId: "root", items: [], counts: { open: 0, unread: 0, history: 0 }, hasMoreBefore: false }, response: { status: 200 } });
		if (path.endsWith("/questions")) {
			const items = [...questions.values()].filter((q) => q.originSessionId === request.params.path.sessionId && (request.params.query.state === "open" ? q.state === "open" : q.state !== "open"));
			return Promise.resolve({ data: { items, open: items.length, unread: items.filter((q) => !q.read).length, history: 0, hasMoreBefore: false, nextBeforeSequence: 0 }, response: { status: 200 } });
		}
		const q = questions.get(request.params.path.questionId);
		return Promise.resolve(q?.originSessionId === request.params.path.sessionId ? { data: q, response: { status: 200 } } : { error: { code: "QUESTION_NOT_FOUND" }, response: { status: 404 } });
	});
	post.mockImplementation((_host, path, request) => {
		const q = questions.get(request.params.path.questionId);
		if (!q) return Promise.resolve({ response: { status: 204 } });
		if (path.endsWith("/read")) { questions.set(q.id, { ...q, read: true }); return Promise.resolve({ response: { status: 204 } }); }
		const receipt = { clientAnswerId: request.body.clientAnswerId, questionVersion: request.body.expectedVersion, answers: request.body.answers, savedAt: "2026-11-07T12:00:00Z", deliveryState: "queued" } satisfies NonNullable<ReturnType<typeof sessionQuestionFixture>["answer"]>;
		questions.set(q.id, { ...q, state: "answered", answer: receipt });
		return Promise.resolve({ data: receipt, response: { status: 200 } });
	});
});
afterEach(() => { cleanup(); client.clear(); surface.remove(); vi.clearAllMocks(); });
async function chooseAnswers() {
	await userEvent.click(await screen.findByRole("radio", { name: /Staging/ }));
	await userEvent.click(screen.getByRole("checkbox", { name: "Tests" }));
}
function delayOriginRead() {
	const original = get.getMockImplementation();
	const question = questions.get(ref.questionId); if (!question) throw new Error("missing fixture");
	const snapshot = structuredClone(question);
	let finish = () => {};
	let holding = true;
	let signal: AbortSignal | undefined;
	const response = new Promise((resolve) => { finish = () => resolve({ data: snapshot, response: { status: 200 } }); });
	get.mockImplementation((host, path, request) => {
		if (holding && path.endsWith("/questions/{questionId}") && request.params.path.questionId === ref.questionId) {
			holding = false;
			signal = request.signal;
			return response;
		}
		return original?.(host, path, request);
	});
	return { release: finish, get signal() { return signal; } };
}

it("restores the routed question row's focus after closing without losing its draft", async () => {
	show("/projects/project/sessions/main");
	const row = await screen.findByRole("button", { name: /Open decision: worker decision/ });
	await userEvent.click(row); await chooseAnswers();
	const draft = structuredClone(Object.values(useQuestionDraftStore.getState().entries)[0]);
	await userEvent.keyboard("{Escape}");
	await waitFor(() => expect(screen.queryByRole("dialog", { name: "Decision" })).not.toBeInTheDocument());
	await waitFor(() => expect(row).toHaveFocus());
	expect(Object.values(useQuestionDraftStore.getState().entries)).toEqual([draft]);
	expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(0);
});

it("keeps the Sheet closed after a routed dialog falls below its size floor", async () => {
	frameRect = { left: 16, top: 48, width: 278, height: 188 };
	surface.getBoundingClientRect = () => new DOMRect(0, 0, 310, 380);
	show("/projects/project/sessions/main");
	await userEvent.click(await screen.findByRole("button", { name: "Awaiting you 3", expanded: false }));
	const sheet = await screen.findByRole("dialog", { name: "Awaiting you" });
	await userEvent.click(within(sheet).getByRole("button", { name: /Open decision: worker decision/ }));
	await chooseAnswers();
	expect(screen.queryByRole("dialog", { name: "Awaiting you" })).not.toBeInTheDocument();
	const draft = structuredClone(Object.values(useQuestionDraftStore.getState().entries)[0]);
	frameRect = { left: 16, top: 48, width: 218, height: 144 };
	surface.getBoundingClientRect = () => new DOMRect(0, 0, 250, 320);
	act(() => { window.dispatchEvent(new Event("resize")); });
	await waitFor(() => expect(screen.queryAllByRole("dialog")).toHaveLength(0));
	const entry = screen.getByRole("button", { name: "Awaiting you 3", expanded: false });
	expect(entry).toBeVisible();
	await waitFor(() => expect([entry, screen.getByRole("button", { name: "Open Awaiting you" })]).toContain(document.activeElement));
	expect(Object.values(useQuestionDraftStore.getState().entries)).toEqual([draft]);
	expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(0);
});

it("uses complete project counts and pages at the main, saves the worker answer once and synchronizes both views", async () => {
	const { router } = show("/projects/project/sessions/main");
	const frame = await screen.findByRole("region", { name: "Awaiting you" });
	expect(await within(frame).findByText("3", { exact: true })).toBeVisible();
	expect(within(frame).getAllByRole("button", { name: /Open decision: main decision/ })).toHaveLength(1);
	await userEvent.click(within(frame).getByRole("button", { name: "Load older requests" }));
	await within(frame).findByRole("button", { name: /Open decision: direct decision/ });
	await userEvent.click(within(frame).getByRole("button", { name: /Open decision: worker decision/ }));
	await chooseAnswers(); expect(screen.getByText(/report-worker/)).toBeVisible();
	expect(screen.getAllByRole("dialog", { name: "Decision" })).toHaveLength(1);
	await userEvent.click(screen.getByRole("button", { name: "Submit all answers" }));
	await waitFor(() => expect(screen.queryByRole("dialog", { name: "Decision" })).not.toBeInTheDocument());
	await waitFor(() => expect(within(frame).getByText("2", { exact: true })).toBeVisible());
	const sends = post.mock.calls.filter(([, path]) => path.endsWith("/answer"));
	expect(sends).toHaveLength(1); expect(sends[0][2].params.path).toEqual({ sessionId: "worker", questionId: "ask-worker" });
	expect(sends[0][2].body.answers).toHaveLength(2);
	expect(client.getQueryData(questionSourceQueryOptions({ sessionId: "worker", questionId: "ask-worker" }).queryKey)?.answer?.deliveryState).toBe("queued");
	expect(router.state.location.pathname).toBe("/projects/project/sessions/main");
});

it.each([
	{ complete: false, data: {} },
	{ complete: true, data: { sessions: [] } },
])("uses the real workspace producer to gate Q2's no-main answer exception, complete=$complete", async ({ complete, data }) => {
	const original = get.getMockImplementation();
	get.mockImplementation((host, path, request) => {
		if (path === "/api/v1/projects") return Promise.resolve({ data: { projects: [{ id: "project", name: "Project", path: "/fixture" }] }, response: { status: 200 } });
		if (path === "/api/v1/sessions") return Promise.resolve({ data, response: { status: 200 } });
		return original?.(host, path, request);
	});
	show(questionURL("/projects/project"), {}, true);
	await waitFor(() => expect(screen.getByTestId("workspace-query")).toHaveTextContent(/idle$/));
	const choice = await screen.findByRole("radio", { name: /Staging/ });
	if (complete) {
		expect(choice).toBeEnabled();
		await chooseAnswers();
		expect(screen.getByRole("button", { name: "Submit all answers" })).toBeEnabled();
	} else {
		expect(choice).toBeDisabled();
		expect(screen.getByText(/Loading the current orchestrator/)).toBeVisible();
		expect(screen.getByRole("button", { name: "Submit all answers" })).toBeDisabled();
	}
	expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(0);
});

it("keeps unknown paused, permits the confirmed no-main fallback and preserves one draft across both host transitions", async () => {
	const app = show("/projects/project", { localReady: false, workspaces: [workspace([session("worker", "worker")])] });
	await userEvent.click(await screen.findByRole("button", { name: "Open project attention" }));
	await userEvent.click(await screen.findByRole("button", { name: /worker decision/ }));
	const stage = await screen.findByRole("radio", { name: /Staging/ });
	expect(stage).toBeDisabled(); expect(screen.getByText(/Loading the current orchestrator/)).toBeVisible();
	app.update({ localReady: true }); await waitFor(() => expect(screen.getByRole("radio", { name: /Staging/ })).toBeEnabled());
	await chooseAnswers();
	app.update({ workspaces: [workspace([session("main"), session("worker", "worker")])] });
	await waitFor(() => expect(app.router.state.location.pathname).toBe("/projects/project/sessions/main"));
	expect(screen.getAllByRole("dialog", { name: "Decision" })).toHaveLength(1);
	expect(screen.getByRole("radio", { name: /Staging/ })).toBeChecked();
	app.update({ localReady: false, workspaces: [workspace([session("main", "orchestrator", true), session("worker", "worker")])] });
	await waitFor(() => expect(screen.getByRole("radio", { name: /Staging/ })).toBeDisabled());
	expect(app.router.state.location.pathname).toBe("/projects/project/sessions/main");
	app.update({ localReady: true });
	await waitFor(() => expect(app.router.state.location.pathname).toBe("/projects/project"));
	expect(screen.getByRole("radio", { name: /Staging/ })).toBeChecked();
	expect(screen.getByRole("button", { name: "Submit all answers" })).toBeEnabled();
	expect(app.router.state.location.search.question).toEqual(ref);
	expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(0);
});

it.each([{ outcome: "delayed success" }, { outcome: "failed rollback" }])("keeps a pending main kill unknown until the daemon settles it, $outcome", async ({ outcome }) => {
	const facts = (terminated: boolean) => ({ data: { sessions: [
		{ id: "main", projectId: "project", kind: "orchestrator", isTerminated: terminated, createdAt: "2026-10-01T00:00:00Z", updatedAt: "2026-10-31T00:00:00Z", status: terminated ? "terminated" : "idle", activity: { state: "idle", lastActivityAt: "2026-10-31T00:00:00Z" } },
		{ id: "worker", projectId: "project", kind: "worker", isTerminated: false, createdAt: "2026-10-31T00:00:00Z", updatedAt: "2026-10-31T00:00:00Z", status: "working", activity: { state: "idle", lastActivityAt: "2026-10-31T00:00:00Z" } },
	] }, response: { status: 200 } });
	let sessions = facts(false);
	const original = get.getMockImplementation();
	get.mockImplementation((host, path, request) => {
		if (path === "/api/v1/projects") return Promise.resolve({ data: { projects: [{ id: "project", name: "Project", path: "/fixture" }] }, response: { status: 200 } });
		if (path === "/api/v1/sessions") return Promise.resolve(sessions);
		return original?.(host, path, request);
	});
	let settle: (result: unknown) => void = () => {};
	const answer = post.getMockImplementation();
	post.mockImplementation((host, path, request) => path.endsWith("/kill")
		? new Promise((resolve) => { settle = resolve; }) : answer?.(host, path, request));
	const app = show(questionURL("/projects/project/sessions/main"), {}, true);
	await waitFor(() => expect(screen.getByRole("radio", { name: /Staging/ })).toBeEnabled());
	act(() => killMain?.());
	await waitFor(() => expect(post.mock.calls.some(([, path]) => path.endsWith("/kill"))).toBe(true));
	await waitFor(() => expect(screen.getByRole("radio", { name: /Staging/ })).toBeDisabled());
	expect(screen.getByText(/Loading the current orchestrator/)).toBeVisible();
	expect(app.router.state.location.pathname).toBe("/projects/project/sessions/main");
	if (outcome === "delayed success") {
		sessions = facts(true);
		await act(async () => settle({ data: {}, response: { status: 200 } }));
		await waitFor(() => expect(app.router.state.location.pathname).toBe("/projects/project"));
	} else {
		await act(async () => settle({ error: { message: "kill failed" }, response: { status: 500 } }));
		await waitFor(() => expect(screen.queryByText(/Loading the current orchestrator/)).toBeNull());
		expect(app.router.state.location.pathname).toBe("/projects/project/sessions/main");
	}
	await waitFor(() => expect(screen.getByRole("radio", { name: /Staging/ })).toBeEnabled());
	expect(app.router.state.location.search.question).toEqual(ref);
	expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(0);
});

it("keeps a submitted answer alive through handover and a project switch without reopening or sending twice", async () => {
	const app = show(questionURL("/projects/project/sessions/main")); await chooseAnswers();
	let finish = () => {};
	post.mockImplementation((_host, path, request) => {
		if (!path.endsWith("/answer")) return Promise.resolve({ response: { status: 204 } });
		return new Promise((resolve) => { finish = () => {
			const receipt = { clientAnswerId: request.body.clientAnswerId, questionVersion: request.body.expectedVersion, answers: request.body.answers, savedAt: "2026-11-07T12:00:00Z", deliveryState: "queued" } satisfies NonNullable<ReturnType<typeof sessionQuestionFixture>["answer"]>;
			const q = questions.get(request.params.path.questionId); if (!q) throw new Error("missing fixture");
			questions.set(q.id, { ...q, state: "answered", answer: receipt }); resolve({ data: receipt, response: { status: 200 } });
		}; });
	});
	await userEvent.click(screen.getByRole("button", { name: "Submit all answers" }));
	app.update({ workspaces: [workspace([session("main", "orchestrator", true), session("new"), session("worker", "worker")])] });
	await waitFor(() => expect(app.router.state.location.pathname).toBe("/projects/project/sessions/new"));
	expect(screen.getByRole("radio", { name: /Staging/ })).toBeChecked();
	const request = post.mock.calls.find(([, path]) => path.endsWith("/answer"))?.[2];
	expect(request.signal.aborted).toBe(false);
	await act(async () => { await app.router.navigate({ to: "/projects/$projectId", params: { projectId: "other" }, search: {} }); });
	await act(async () => finish());
	await waitFor(() => expect(Object.keys(useQuestionDraftStore.getState().entries)).toHaveLength(0));
	expect(app.router.state.location.pathname).toBe("/projects/other");
	expect(screen.queryByRole("dialog", { name: "Decision" })).not.toBeInTheDocument();
	expect(request.signal.aborted).toBe(false); expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(1);
});

it("shows the saved receipt when it cancels a delayed origin read during handover", async () => {
	const app = show(questionURL("/projects/project/sessions/main")); await chooseAnswers();
	const original = post.getMockImplementation();
	let finishAnswer = () => {};
	post.mockImplementation((host, path, request) => {
		if (!path.endsWith("/answer")) return original?.(host, path, request);
		return new Promise((resolve) => { finishAnswer = () => resolve(original?.(host, path, request)); });
	});
	await userEvent.click(screen.getByRole("button", { name: "Submit all answers" }));
	const request = post.mock.calls.find(([, path]) => path.endsWith("/answer"))?.[2];
	const draft = Object.values(useQuestionDraftStore.getState().entries)[0];
	expect(draft.phase).toMatchObject({ state: "submitting", answer: { clientAnswerId: request.body.clientAnswerId } });
	const delayed = delayOriginRead();
	app.update({ workspaces: [workspace([session("main", "orchestrator", true), session("new"), session("worker", "worker")])] });
	await waitFor(() => expect(app.router.state.location.pathname).toBe("/projects/project/sessions/new"));
	await waitFor(() => expect(delayed.signal?.aborted).toBe(false));
	expect(Object.values(useQuestionDraftStore.getState().entries)).toEqual([draft]);
	await act(async () => finishAnswer());
	await waitFor(() => expect(Object.keys(useQuestionDraftStore.getState().entries)).toHaveLength(0));
	expect(delayed.signal?.aborted).toBe(true);
	const options = questionSourceQueryOptions({ sessionId: ref.originSessionId, questionId: ref.questionId });
	expect(client.getQueryData(options.queryKey)?.answer?.clientAnswerId).toBe(request.body.clientAnswerId);
	await screen.findByText("Saved · Pending delivery");
	expect(screen.queryByText("Could not load the latest request. Your draft is kept.")).not.toBeInTheDocument();
	expect(screen.queryByRole("button", { name: "Submit all answers" })).not.toBeInTheDocument();
	expect(screen.getByRole("radio", { name: /Staging/ })).toBeChecked();
	expect(screen.getByRole("radio", { name: /Staging/ })).toBeDisabled();
	expect(screen.getByRole("checkbox", { name: "Tests" })).toBeChecked();
	await act(async () => delayed.release());
	expect(client.getQueryData(options.queryKey)).toMatchObject({ state: "answered", answer: { clientAnswerId: request.body.clientAnswerId } });
	expect(Object.keys(useQuestionDraftStore.getState().entries)).toHaveLength(0);
	expect(request.signal.aborted).toBe(false);
	expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(1);
	expect(request.params.path).toEqual({ sessionId: "worker", questionId: "ask-worker" });
});

it.each(["editing", "unknown"] as const)("keeps a canceled handover read unavailable without a saved receipt and retains the %s draft", async (phase) => {
	const app = show(questionURL("/projects/project/sessions/main")); await chooseAnswers();
	if (phase === "unknown") {
		const original = post.getMockImplementation();
		post.mockImplementation((host, path, request) => path.endsWith("/answer") ? Promise.reject(new Error("response lost")) : original?.(host, path, request));
		await userEvent.click(screen.getByRole("button", { name: "Submit all answers" }));
		await waitFor(() => expect(Object.values(useQuestionDraftStore.getState().entries)[0]?.phase.state).toBe("unknown"));
	}
	const draft = structuredClone(Object.values(useQuestionDraftStore.getState().entries)[0]);
	const delayed = delayOriginRead();
	app.update({ workspaces: [workspace([session("main", "orchestrator", true), session("new"), session("worker", "worker")])] });
	await waitFor(() => expect(app.router.state.location.pathname).toBe("/projects/project/sessions/new"));
	await waitFor(() => expect(delayed.signal?.aborted).toBe(false));
	await act(async () => client.cancelQueries({ queryKey: questionSourceQueryOptions({ sessionId: ref.originSessionId, questionId: ref.questionId }).queryKey, exact: true }));
	await screen.findByText("Could not load the latest request. Your draft is kept.");
	expect(screen.queryByText("Saved · Pending delivery")).not.toBeInTheDocument();
	expect(screen.queryByRole("button", { name: "Submit all answers" })).not.toBeInTheDocument();
	expect(Object.values(useQuestionDraftStore.getState().entries)).toEqual([draft]);
	expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(phase === "unknown" ? 1 : 0);
	await act(async () => delayed.release());
});

it("does not treat partially loaded or failed remote facts as no orchestrator or use another host's main", async () => {
	const remoteRef = { ...ref, hostId: "host-b" };
	const app = show(questionURL("/host/host-b/project/project", remoteRef), { remoteWorkspaces: [workspace([session("wrong")], "host-a"), workspace([session("worker", "worker")], "host-b")], loadedProjectHostIds: ["host-b"] });
	expect(await screen.findByRole("radio", { name: /Staging/ })).toBeDisabled();
	app.update({ loadedSessionHostIds: ["host-b"], failedHostIds: ["host-b"] });
	expect(screen.getByRole("radio", { name: /Staging/ })).toBeDisabled();
	app.update({ failedHostIds: [] });
	await waitFor(() => expect(screen.getByRole("radio", { name: /Staging/ })).toBeEnabled());
	expect(app.router.state.location.pathname).toBe("/host/host-b/project/project");
	app.update({ remoteWorkspaces: [workspace([session("wrong")], "host-a"), workspace([session("remote-main")], "host-b")] });
	await waitFor(() => expect(app.router.state.location.pathname).toBe("/host/host-b/project/project/session/remote-main"));
	expect(get.mock.calls.filter(([, path]) => path.endsWith("/questions/{questionId}")).every(([host]) => host === "host-b")).toBe(true);
});

it("rejects a mismatched project before showing controls or marking the source read", async () => {
	const q = questions.get("ask-worker"); if (!q) throw new Error("missing fixture"); questions.set(q.id, { ...q, projectId: "other" });
	show(questionURL("/projects/project/sessions/main"));
	await screen.findByText("This host returned an incomplete or unsupported question. Reload before answering.");
	expect(screen.queryByRole("radio")).not.toBeInTheDocument(); expect(post).not.toHaveBeenCalled();
});

it("keeps an expired origin read-only even with the confirmed no-main exception", async () => {
	const q = questions.get("ask-worker"); if (!q) throw new Error("missing fixture"); questions.set(q.id, { ...q, state: "expired", closedAt: "2026-11-07T12:00:00Z", closeReason: "origin_terminated" });
	show(questionURL("/projects/project"), { workspaces: [workspace([session("worker", "worker", true)])] });
	expect(await screen.findByRole("radio", { name: /Staging/ })).toBeDisabled();
	expect(screen.queryByRole("button", { name: "Submit all answers" })).not.toBeInTheDocument(); expect(post).not.toHaveBeenCalled();
});

it("rechecks an origin that expired while its presenter was unknown before enabling answers", async () => {
	const app = show(questionURL("/projects/project"), { localReady: false, workspaces: [workspace([session("worker", "worker")])] });
	expect(await screen.findByRole("radio", { name: /Staging/ })).toBeDisabled();
	const question = questions.get("ask-worker"); if (!question) throw new Error("missing fixture");
	questions.set(question.id, { ...question, state: "expired", closedAt: "2026-11-07T12:00:00Z", closeReason: "origin_terminated" });
	app.update({ localReady: true });
	await within(screen.getByRole("dialog", { name: "Decision" })).findByText("Expired", { exact: true });
	expect(screen.getByRole("radio", { name: /Staging/ })).toBeDisabled();
	expect(screen.queryByRole("button", { name: "Submit all answers" })).not.toBeInTheDocument();
	expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(0);
});

it("keeps rejected old choices as reference and opens the new source version without applying them", async () => {
	show(questionURL("/projects/project/sessions/main")); await chooseAnswers();
	const original = post.getMockImplementation();
	post.mockImplementation((host, path, request) => {
		if (!path.endsWith("/answer")) return original?.(host, path, request);
		const question = questions.get("ask-worker"); if (!question) throw new Error("missing fixture");
		questions.set(question.id, { ...question, version: 2, read: false, payload: { ...question.payload, explanation: "Review version two before answering." } });
		return Promise.resolve({ error: { code: "QUESTION_VERSION_CONFLICT", message: "Question changed" }, response: { status: 409 } });
	});
	await userEvent.click(screen.getByRole("button", { name: "Submit all answers" }));
	await screen.findByText("Review version two before answering.");
	await waitFor(() => expect(screen.getByRole("radio", { name: /Staging/ })).not.toBeChecked());
	expect(screen.getByRole("checkbox", { name: "Tests" })).not.toBeChecked();
	await userEvent.click(screen.getByText("Previous draft · version 1 · reference only"));
	expect(screen.getByText("This draft is a reference. Choices are not copied into the current version.")).toBeVisible();
	expect(post.mock.calls.filter(([, path]) => path.endsWith("/answer"))).toHaveLength(1);
	expect(post.mock.calls.find(([, path]) => path.endsWith("/answer"))?.[2].body.expectedVersion).toBe(1);
});

it("opens a worker provider summary with its original conversation and branch, then navigates to that fixed session", async () => {
	providerRow = true; const app = show("/projects/project/sessions/main");
	await userEvent.click(await screen.findByRole("button", { name: /Open decision: Worker provider request/ }));
	await userEvent.click(await screen.findByRole("button", { name: "Go to original request" }));
	await waitFor(() => expect(app.router.state.location.pathname).toBe("/projects/project/sessions/worker"));
	expect(get).toHaveBeenCalledWith(undefined, "/api/v1/sessions/{sessionId}/attention/{activityId}", expect.objectContaining({ params: { path: { sessionId: "worker", activityId: "p" } } }));
	expect(post.mock.calls.every(([, path]) => path.endsWith("/read"))).toBe(true);
	expect(app.router.state.location.search.followOrchestrator).toBeUndefined();
});
