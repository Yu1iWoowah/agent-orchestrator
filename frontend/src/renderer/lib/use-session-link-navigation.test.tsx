import { DocumentReadingContext } from "./document-reading-context";
import { useDocumentReadingStore } from "../stores/document-reading-store";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useUiStore } from "../stores/ui-store";
import { useSessionLinkNavigation } from "./use-session-link-navigation";

const mocks = vi.hoisted(() => ({ navigate: vi.fn(), workspace: vi.fn(), cloudProjects: vi.fn(), cloudSessions: vi.fn() }));
vi.mock("./navigate-to-session", () => ({ useNavigateToSession: () => mocks.navigate }));
vi.mock("../hooks/useWorkspaceQuery", () => ({
	useWorkspaceQuery: () => mocks.workspace(),
	useCloudProjectsQuery: () => mocks.cloudProjects(),
	useCloudSessionsQuery: () => mocks.cloudSessions(),
	toCloudWorkspace: (project: { id: string; displayName?: string }, sessions: Array<{ id: string; projectId: string }>) => ({
		id: project.id,
		name: project.displayName ?? project.id,
		path: "",
		sessions: sessions.filter((session) => session.projectId === project.id),
	}),
	remoteWorkspaceQueryKey: (hostId: string) => ["remote-workspaces", hostId],
}));
vi.mock("../hooks/useCloudOrg", () => ({ useCloudOrg: () => ({ ready: true, org: { id: "org-1" } }) }));

describe("useSessionLinkNavigation", () => {
	beforeEach(() => {
		mocks.navigate.mockReset();
		useDocumentReadingStore.setState({sessions:{},reader:null});
		mocks.workspace.mockClear();
		mocks.cloudProjects.mockClear();
		mocks.cloudSessions.mockClear();
		mocks.cloudProjects.mockReturnValue({ isSuccess: true, data: [] });
		mocks.cloudSessions.mockReturnValue({ isSuccess: true, data: [] });
		useUiStore.setState({ globalToast: null, globalToasts: [], globalToastSequence: 0 });
		mocks.workspace.mockReturnValue({
			isSuccess: true,
			data: [
				{ id: "other-project", sessions: [{ id: "other-session", isTerminated: false }] },
				{ id: "project", sessions: [{ id: "session", title: "renamed", isTerminated: false }, { id: "terminated", isTerminated: true }] },
			],
		});
	});

	it("routes documents to this screen's reader instead of the last registered Chat", () => {
		const current = { key: "current", sessionId: "orchestrator", connection: { kind: "local", identity: "local" } } as const;
		useDocumentReadingStore.getState().register({ ...current, key: "hidden-chat" });
		const wrapper = ({ children }: { children: ReactNode }) => <DocumentReadingContext.Provider value={current}>{children}</DocumentReadingContext.Provider>;
		const { result } = renderHook(() => useSessionLinkNavigation(), { wrapper });
		for (const url of ["ao://sessions/other-project/other-session/artifacts/a.md", "ao://projects/other-project/shared/a.md", "/managed/a.md", "file:///managed/a.md"]) {
			act(() => expect(result.current(url)).toBe(true));
			expect(useDocumentReadingStore.getState().sessions.current?.opening).toBeDefined();
			expect(useDocumentReadingStore.getState().sessions.current?.opening).not.toBeNull();
		}
		expect(useDocumentReadingStore.getState().sessions["hidden-chat"]?.opening).toBeUndefined();
		expect(mocks.navigate).not.toHaveBeenCalled();
		act(() => expect(result.current("ao://sessions/other-project/other-session")).toBe(true));
		expect(mocks.navigate).toHaveBeenCalledWith("other-project", "other-session");
	});

	it("uses the current screen's own viewer outside a reader context", () => {
		useDocumentReadingStore.getState().register({ key: "hidden-chat", sessionId: "hidden", connection: { kind: "local", identity: "local" } });
		useUiStore.setState({ workspaceFileOpenRequest: null, projectDocumentTarget: null });
		const { result } = renderHook(() => useSessionLinkNavigation());
		act(() => expect(result.current("ao://projects/project/shared/guide.md")).toBe(true));
		expect(useUiStore.getState().projectDocumentTarget).toEqual({ kind: "shared", projectId: "project", path: "guide.md" });
		act(() => expect(result.current("ao://sessions/project/session/artifacts/guide.md")).toBe(true));
		expect(useUiStore.getState().workspaceFileOpenRequest).toEqual(expect.objectContaining({ sessionId: "session", path: "guide.md", source: "artifact" }));
		expect(mocks.navigate).toHaveBeenCalledWith("project", "session");
		expect(useDocumentReadingStore.getState().sessions["hidden-chat"]?.opening).toBeUndefined();
	});

 it("opens project files without selecting a source session", () => {
 useUiStore.setState({ projectDocumentTarget: null });
 const { result } = renderHook(() => useSessionLinkNavigation());
 act(() => expect(result.current("ao://projects/project/shared/handoff/doc.md")).toBe(true));
 expect(useUiStore.getState().projectDocumentTarget).toEqual({ kind: "shared", projectId: "project", path: "handoff/doc.md" });
 expect(mocks.navigate).not.toHaveBeenCalled();
 });
 it("rejects inaccessible project documents without falling back", () => {
 useUiStore.setState({ projectDocumentTarget: null });
 const { result } = renderHook(() => useSessionLinkNavigation());
 act(() => expect(result.current("ao://projects/missing/shared/doc.md")).toBe(false));
 expect(useUiStore.getState().projectDocumentTarget).toBeNull();
 expect(mocks.navigate).not.toHaveBeenCalled();
 });

	it("selects the exact cross-project session by stable ID", () => {
		const { result } = renderHook(() => useSessionLinkNavigation());
		act(() => expect(result.current("ao://sessions/other-project/other-session")).toBe(true));
		expect(mocks.navigate).toHaveBeenCalledWith("other-project", "other-session");
		expect(useUiStore.getState().globalToasts).toEqual([]);
	});

	it("resolves a remote chat link on its source host without querying local or Cloud", () => {
		mocks.workspace.mockReturnValue({ isSuccess: true, data: [{ id: "project", sessions: [{ id: "session", isTerminated: true }] }] });
		const queryClient = new QueryClient();
		queryClient.setQueryData(["remote-workspaces", "box-a"], [{ id: "project", sessions: [{ id: "session", isTerminated: false }] }]);
		queryClient.setQueryData(["remote-workspaces", "box-b"], [{ id: "project", sessions: [{ id: "session", isTerminated: true }] }]);
		const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
		const { result } = renderHook(() => useSessionLinkNavigation("box-a"), { wrapper });
		act(() => expect(result.current("ao://sessions/project/session")).toBe(true));
		expect(mocks.navigate).toHaveBeenCalledWith("project", "session", "box-a");
		expect(mocks.workspace).not.toHaveBeenCalled();
		expect(mocks.cloudProjects).not.toHaveBeenCalled();
		expect(mocks.cloudSessions).not.toHaveBeenCalled();
	});

	it("resolves a Cloud chat link without querying local or remote workspaces", () => {
			mocks.workspace.mockReturnValue({ isSuccess: false, data: undefined });
			mocks.cloudProjects.mockReturnValue({ isSuccess: true, data: [{ id: "project" }] });
			mocks.cloudSessions.mockReturnValue({ isSuccess: true, data: [{ id: "session", projectId: "project", isTerminated: false }] });
			const { result } = renderHook(() => useSessionLinkNavigation(undefined, "cloud"));
			act(() => expect(result.current("ao://sessions/project/session")).toBe(true));
			expect(mocks.navigate).toHaveBeenCalledWith("project", "session");
			expect(useUiStore.getState().globalToasts).toEqual([]);
			expect(mocks.workspace).not.toHaveBeenCalled();
	});

	it("shows feedback instead of navigating to a terminated session", () => {
		const { result } = renderHook(() => useSessionLinkNavigation());
		act(() => expect(result.current("ao://sessions/project/terminated")).toBe(false));
		act(() => expect(result.current("ao://sessions/project/terminated")).toBe(false));
		expect(mocks.navigate).not.toHaveBeenCalled();
		expect(useUiStore.getState().globalToasts).toEqual([
			expect.objectContaining({
				title: "Session terminated is terminated",
				nonce: 2,
				placement: "top-center",
				dismissible: true,
				durationMs: 5_000,
				dedupeKey: "session-link:project:terminated",
			}),
		]);
	});

	it("navigates to the session and requests the artifact open, even after termination", () => {
		useUiStore.setState({ workspaceFileOpenRequest: null });
		const { result } = renderHook(() => useSessionLinkNavigation());
		act(() => expect(result.current("ao://sessions/project/terminated/artifacts/plans/my%20plan.md")).toBe(true));
		expect(mocks.navigate).toHaveBeenCalledWith("project", "terminated");
		expect(useUiStore.getState().workspaceFileOpenRequest).toEqual(expect.objectContaining({
			sessionId: "terminated",
			path: "plans/my plan.md",
			source: "artifact",
		}));
		expect(useUiStore.getState().globalToasts).toEqual([]);
	});

	it.each([
		["ao://sessions/project/missing", "missing or is not accessible"],
		["ao://sessions/project/session/kill", "malformed or unsupported"],
	])("rejects %s with actionable feedback", (url, message) => {
		const { result } = renderHook(() => useSessionLinkNavigation());
		act(() => expect(result.current(url)).toBe(false));
		expect(mocks.navigate).not.toHaveBeenCalled();
		expect(useUiStore.getState().globalToasts.at(-1)).toEqual(expect.objectContaining({
			title: expect.stringContaining(message),
			tone: "error",
			placement: "top-center",
			dismissible: true,
			dedupeKey: "session-link:error",
		}));
	});

	it("does not navigate when the workspace cannot be verified", () => {
		mocks.workspace.mockReturnValue({ isSuccess: false, data: undefined });
		const { result } = renderHook(() => useSessionLinkNavigation());
		act(() => expect(result.current("ao://sessions/project/session")).toBe(false));
		expect(useUiStore.getState().globalToasts.at(-1)?.title).toContain("daemon connection");
	});
});
