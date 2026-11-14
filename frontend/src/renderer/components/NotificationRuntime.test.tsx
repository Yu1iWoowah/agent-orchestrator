import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { unreadNotificationsQueryKey, type NotificationDTO, type NotificationsCache } from "../lib/notifications";
import { refKey } from "../lib/hosts";
import { remoteNotificationsQueryKey } from "../hooks/useRemoteNotifications";
import { NotificationRuntime } from "./NotificationCenter";

const notifications: NotificationDTO[] = [
	{
		id: "ntf_1",
		sessionId: "sess-1",
		projectId: "proj-1",
		prUrl: "",
		type: "needs_input",
		title: "Needs input",
		body: "",
		status: "unread",
		createdAt: "2026-06-16T10:00:00Z",
		target: { kind: "session", sessionId: "sess-1" },
	},
	{
		id: "ntf_2",
		sessionId: "sess-2",
		projectId: "proj-1",
		prUrl: "",
		type: "ready_to_merge",
		title: "Ready to merge",
		body: "",
		status: "unread",
		createdAt: "2026-06-16T11:00:00Z",
		target: { kind: "session", sessionId: "sess-2" },
	},
];

const unreadCache: NotificationsCache = {
	pages: [{ notifications, nextCursor: undefined, unreadCount: notifications.length, unresolvedCount: notifications.length }],
	pageParams: [""],
};

const { setBadge, onClick, openQuestion, navigate } = vi.hoisted(() => ({ setBadge: vi.fn(), onClick: vi.fn<(callback: (id: string) => void) => () => void>(() => () => undefined), openQuestion: vi.fn(), navigate: vi.fn() }));

vi.mock("@tanstack/react-router", () => ({ useNavigate: () => navigate, useParams: () => ({}) }));
vi.mock("./ProjectQuestionRouting", () => ({ useProjectQuestionRouting: () => ({ openQuestion }) }));

vi.mock("../hooks/useNotificationsQuery", () => ({
	useNotificationsQuery: () => ({ data: unreadCache, isError: false }),
}));

vi.mock("../lib/notifications", async (importOriginal) => ({
	...((await importOriginal()) as object),
	createNotificationsTransport: () => ({ connect: () => undefined }),
}));

vi.mock("../lib/bridge", () => ({
	aoBridge: {
		notifications: {
			setBadge,
			show: vi.fn(),
			devBounce: vi.fn(),
			onClick,
		},
	},
}));

function renderRuntimeOnly(queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
	return render(
		<QueryClientProvider client={queryClient}>
			{/* NotificationCenter (the bell) is intentionally NOT mounted here. */}
			<NotificationRuntime />
		</QueryClientProvider>,
	);
}

describe("NotificationRuntime badge sync", () => {
	it("syncs the launcher badge without the notification bell being mounted", async () => {
		setBadge.mockClear();

		renderRuntimeOnly();

		// The badge must update purely from the always-mounted runtime, independent
		// of NotificationCenter, which Linux hides from the topbar and only mounts
		// on the sessions board.
		await waitFor(() => expect(setBadge).toHaveBeenCalledWith(notifications.length));
	});

	it.each(["local", "box-a"])("opens an OS question notification on %s through the same immutable source reference", async (hostId) => {
		openQuestion.mockClear(); navigate.mockClear(); onClick.mockClear();
		const notification: NotificationDTO = { ...notifications[0], id: "ntf_question", target: { kind: "agent_question", sessionId: "sess-1", questionId: "ask-worker" } };
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		if (hostId === "local") client.setQueryData(unreadNotificationsQueryKey, { ...unreadCache, pages: [{ notifications: [notification], unreadCount: 1, unresolvedCount: 1 }] } satisfies NotificationsCache);
		else client.setQueryData(remoteNotificationsQueryKey(hostId, "unread"), { notifications: [notification], unreadCount: 1, unresolvedCount: 1 });
		renderRuntimeOnly(client);
		const callback = onClick.mock.calls.at(-1)?.[0];
		act(() => callback?.(hostId === "local" ? notification.id : `remote-notification:${refKey({ host: hostId, id: notification.id })}`));
		expect(openQuestion).toHaveBeenCalledWith({ hostId, projectId: "proj-1", originSessionId: "sess-1", questionId: "ask-worker" });
		expect(navigate).not.toHaveBeenCalled();
	});
});
