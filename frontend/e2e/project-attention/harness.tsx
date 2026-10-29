import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
	createMemoryHistory,
	createRootRoute,
	createRoute,
	createRouter,
	Outlet,
	RouterProvider,
} from "@tanstack/react-router";
import { createRoot } from "react-dom/client";
import { I18nextProvider } from "react-i18next";
import { createFullAppI18n } from "../../src/renderer/i18n/all-messages";
import { TooltipProvider } from "../../src/renderer/components/ui/tooltip";
import { sessionQuestionFixture } from "../../src/renderer/test/fixtures/session-question";
import type { components } from "../../src/api/schema";
import "../../src/renderer/styles.css";

// Explicit renderer fixtures; these are not provider approval or native Browser evidence.
const params = new URLSearchParams(location.search);
const queryClient = new QueryClient({
	defaultOptions: { queries: { retry: false } },
});
let question = sessionQuestionFixture({ originSessionId: "worker" });
let providerRead = false,
	reportRead = false,
	offline = false;
const provider = {
	branchId: "root",
	actionable: true,
	read: false,
	activity: {
		kind: "activity",
		id: "approval",
		requestId: "provider-rpc",
		activityKind: "approval",
		status: "pending",
		sequence: 1,
		revision: 0,
		summary: "Allow workspace command?",
		createdAt: "2026-10-29T00:00:00Z",
		detail: {
			message: "Read the workspace",
			decisions: [{ id: "allow", label: "Allow once", kind: "allow_once" }],
		},
	},
};
const originalFetch = window.fetch.bind(window);
window.fetch = async (input, init) => {
	const request =
		input instanceof Request
			? input
			: new Request(new URL(String(input), location.href), init);
	const url = new URL(request.url);
	if (!url.pathname.startsWith("/api/")) return originalFetch(input, init);
	if (url.pathname.endsWith("/projects/project/attention")) {
		if (offline)
			return Response.json(
				{ message: "Fixture host disconnected" },
				{ status: 503 },
			);
		const common = {
			sessionId: "worker",
			sessionName: "A long worker session name",
			createdAt: "2026-10-29T00:00:00Z",
		};
		const human = [
			...(question.state === "open"
				? [
						{
							...common,
							id: `question:${question.id}`,
							kind: "agent_question",
							title: question.payload.title,
							revision: question.version,
							read: question.read,
							questionId: question.id,
						},
					]
				: []),
			{
				...common,
				id: "provider:approval",
				kind: "provider_approval",
				title: "Allow workspace command?",
				revision: 0,
				read: providerRead,
				activityId: "approval",
				requestId: "provider-rpc",
				conversationId: "conversation",
				branchId: "root",
				sourceBranchId: "root",
			},
		];
		const reports = [
			{
				...common,
				id: "report:r1",
				kind: "worker_report",
				title: "Please coordinate this report before involving the user.",
				revision: 1,
				read: reportRead,
				reportId: "r1",
				originHarness: "codex",
				deliveryState: "acknowledged",
				answerState: "unknown",
			},
		];
		const items =
			url.searchParams.get("audience") === "orchestrator" ? reports : human;
		return Response.json({
			items,
			open: items.length,
			unread: items.filter((item) => !item.read).length,
		});
	}
	if (url.pathname.endsWith("/attention/reports/r1/read")) {
		reportRead = true;
		document.documentElement.dataset.reportReads = "1";
		return new Response(null, { status: 204 });
	}
	if (url.pathname.endsWith("/questions/ask-1/read")) {
		question = { ...question, read: true };
		return new Response(null, { status: 204 });
	}
	if (url.pathname.endsWith("/attention/approval/read")) {
		providerRead = true;
		return new Response(null, { status: 204 });
	}
	if (url.pathname.endsWith("/questions/ask-1/answer")) {
		const body: components["schemas"]["AttentionAnswerSubmission"] =
			await request.json();
		const receipt: components["schemas"]["AttentionAnswerReceipt"] = {
			clientAnswerId: body.clientAnswerId,
			questionVersion: body.expectedVersion,
			answers: body.answers,
			savedAt: "2026-10-29T00:02:00Z",
			deliveryState: "queued",
		};
		question = {
			...question,
			state: "answered",
			answer: receipt,
			closedBy: "user",
			closedAt: receipt.savedAt,
			closeReason: "answer_saved",
		};
		document.documentElement.dataset.answerPosts = String(
			Number(document.documentElement.dataset.answerPosts ?? 0) + 1,
		);
		void queryClient.invalidateQueries();
		return Response.json(receipt);
	}
	if (url.pathname.endsWith("/questions/ask-1")) return Response.json(question);
	if ((url.pathname.endsWith("/sessions/worker/attention") || url.pathname.endsWith("/sessions/worker/attention/approval")))
		return Response.json({
			conversationId: "conversation",
			activeBranchId: "root",
			items: [{ ...provider, read: providerRead }],
			counts: { open: 1, unread: providerRead ? 0 : 1, history: 0 },
			hasMoreBefore: false,
		});
	if (url.pathname.includes("/resolve")) {
		document.documentElement.dataset.approvalPosts = "1";
		return Response.json(
			{ message: "Fixture forbids approval" },
			{ status: 400 },
		);
	}
	return Response.json({ message: "No fixture API" }, { status: 404 });
};
const { ProjectAttention, ProjectAttentionCount } =
	await import("../../src/renderer/components/ProjectAttention");
const { QuestionAnswerRuntime } =
	await import("../../src/renderer/hooks/useQuestionAnswerRecovery");
const { setApiBaseUrl } = await import("../../src/renderer/lib/api-client");
setApiBaseUrl(location.origin);
window.addEventListener("project-attention-fixture", (event) => {
	if (!(event instanceof CustomEvent)) return;
	if (event.detail === "offline") offline = true;
	if (event.detail === "revise")
		question = {
			...question,
			version: question.version + 1,
			read: false,
			payload: {
				...question.payload,
				explanation: "A new revision needs your decision.",
			},
		};
	void queryClient.invalidateQueries();
});
function Harness() {
	return (
		<main className="center-panel-surface flex h-screen flex-col bg-background text-foreground">
			<header className="flex h-12 shrink-0 items-center gap-4 border-b px-4">
				<span>Project attention fixture</span>
				<ProjectAttentionCount projectId="project" />
				<ProjectAttention projectId="project" />
			</header>
			<section className="min-h-0 flex-1 p-6">
				<p>Browser fixture stays in its current page.</p>
				<input
					aria-label="Browser address"
					defaultValue="https://example.invalid/fixture"
				/>
			</section>
			<footer className="shrink-0 p-4">
				<textarea
					className="h-20 w-full border bg-card p-3"
					aria-label="Conversation draft"
				/>
			</footer>
		</main>
	);
}
const root = createRootRoute({ component: () => <Outlet /> });
const index = createRoute({
	getParentRoute: () => root,
	path: "/",
	component: Harness,
});
const session = createRoute({
	getParentRoute: () => root,
	path: "/projects/$projectId/sessions/$sessionId",
	component: () => <p>Original worker session</p>,
});
const router = createRouter({
	routeTree: root.addChildren([index, session]),
	history: createMemoryHistory({ initialEntries: ["/"] }),
});
const element = document.getElementById("root");
if (!element) throw new Error("Missing fixture root");
document.documentElement.classList.toggle(
	"dark",
	params.get("theme") !== "light",
);
createRoot(element).render(
	<QueryClientProvider client={queryClient}>
		<I18nextProvider
			i18n={createFullAppI18n(
				params.get("locale") === "zh-TW" ? "zh-TW" : "en",
			)}
		>
			<TooltipProvider>
				<QuestionAnswerRuntime />
				<RouterProvider router={router} />
			</TooltipProvider>
		</I18nextProvider>
	</QueryClientProvider>,
);
