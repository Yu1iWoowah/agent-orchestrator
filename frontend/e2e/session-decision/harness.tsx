import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRef } from "react";
import { createRoot } from "react-dom/client";
import { I18nextProvider } from "react-i18next";
import { createFullAppI18n } from "../../src/renderer/i18n/all-messages";
import { TooltipProvider } from "../../src/renderer/components/ui/tooltip";
import { largeSessionQuestionFixture, sessionQuestionFixture } from "../../src/renderer/test/fixtures/session-question";
import type { components } from "../../src/api/schema";
import "../../src/renderer/styles.css";

// Explicit test fixture: no daemon, provider, local files, or native consent.
const params = new URLSearchParams(location.search);
const sessionId = "decision-fixture";
const sizeLimit = params.has("size-limit");
let question = sizeLimit ? largeSessionQuestionFixture({ id: "ask-1", originSessionId: sessionId, clientKey: "large-0", read: true }) : sessionQuestionFixture({ originSessionId: sessionId });
const background = sizeLimit ? Array.from({ length: 3 }, (_, i) => largeSessionQuestionFixture({ id: `large-${i + 1}`, clientKey: `large-${i + 1}`, sequence: i + 2, originSessionId: sessionId, read: true })) : [];
let answerMode = params.get("answer") ?? "saved";
let posts = 0;
const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const originalFetch = window.fetch.bind(window);
window.fetch = async (input, init) => {
	const request = input instanceof Request ? input : new Request(new URL(String(input), location.href), init);
	const url = new URL(request.url);
	if (!url.pathname.startsWith("/api/")) return originalFetch(input, init);
	if (url.pathname.endsWith("/questions")) {
		const history = url.searchParams.get("state") === "history";
		return Response.json({ items: (question.state === "open") !== history ? [question] : [], open: question.state === "open" ? 1 : 0, unread: question.read ? 0 : 1, history: question.state === "open" ? 0 : 1, hasMoreBefore: false, nextBeforeSequence: 0 });
	}
	if (url.pathname.endsWith("/read")) { question = { ...question, read: true }; return new Response(null, { status: 204 }); }
	if (url.pathname.endsWith("/answer")) {
		posts++;
		document.documentElement.dataset.answerPosts = String(posts);
		const submission = await request.json() as components["schemas"]["AttentionAnswerSubmission"];
		if (answerMode === "rejected") return Response.json({ code: "QUESTION_INVALID", message: "Check the answers", details: { fieldErrors: { "answers.target": "Review the target before submitting." } } }, { status: 400 });
		if (answerMode === "conflict") {
			question = { ...question, version: 2, payload: { ...question.payload, explanation: "The deployment scope changed. Please confirm the new version." } };
			return Response.json({ code: "QUESTION_CHANGED", message: "Changed" }, { status: 409 });
		}
		const receipt: components["schemas"]["AttentionAnswerReceipt"] = { clientAnswerId: submission.clientAnswerId, questionVersion: submission.expectedVersion, answers: submission.answers, savedAt: "2026-10-29T00:01:00Z", deliveryState: "queued" };
		if (answerMode === "conflict-answered" || answerMode === "conflict-cancelled") {
			question = { ...question, state: answerMode === "conflict-answered" ? "answered" : "cancelled",
				answer: answerMode === "conflict-answered" ? { ...receipt, clientAnswerId: "external-answer" } : undefined };
			return Response.json({ code: "QUESTION_CHANGED", message: "Answered elsewhere or closed" }, { status: 409 });
		}
		question = { ...question, state: "answered", answer: receipt, closedAt: receipt.savedAt, closedBy: "user", closeReason: "answer_saved" };
		if (answerMode === "lost") throw new TypeError("Fixture lost response after durable save");
		if (answerMode === "delay") await new Promise((resolve) => setTimeout(resolve, 800));
		return Response.json(receipt);
	}
	const sourceId = url.pathname.split("/questions/")[1];
	const source = sourceId === question.id ? question : background.find((item) => item.id === sourceId);
	if (request.method === "GET" && source) return Response.json(source);
	if (url.pathname.endsWith("/attention")) return Response.json({ conversationId: "fixture", activeBranchId: "root", counts: { open: 0, unread: 0, history: 0 }, items: [], hasMoreBefore: false });
	if (url.pathname.endsWith("/plan")) return Response.json({ state: "absent", conversationId: "fixture", activeBranchId: "root" });
	return Response.json({ code: "NOT_FOUND", message: "No fixture API" }, { status: 404 });
};
const { SessionConversationFrames } = await import("../../src/renderer/components/chat/SessionConversationFrames");
const { QuestionAnswerRuntime } = await import("../../src/renderer/hooks/useQuestionAnswerRecovery");
const { setApiBaseUrl } = await import("../../src/renderer/lib/api-client");
setApiBaseUrl(location.origin);
if (sizeLimit) {
	const { useQuestionDraftStore } = await import("../../src/renderer/stores/question-draft-store");
	for (const item of [question, ...background]) {
		const key = useQuestionDraftStore.getState().ensure(item);
		for (const prompt of item.payload.questions) useQuestionDraftStore.getState().setField(key, prompt.key, { selectedOptionIds: [], otherSelected: true, otherText: "o".repeat(4000) });
	}
}
const i18n = createFullAppI18n(params.get("locale") === "zh-TW" ? "zh-TW" : "en");
document.documentElement.classList.toggle("dark", params.get("theme") !== "light");
window.addEventListener("decision-fixture", (event) => {
	if (!(event instanceof CustomEvent)) return;
	if (event.detail === "delivered" && question.answer) question = { ...question, answer: { ...question.answer, deliveryState: "delivered", deliveredAt: "2026-10-29T00:02:00Z" } };
	if (event.detail === "saved") answerMode = "saved";
	void client.invalidateQueries();
});
function Harness() {
	const surface = useRef<HTMLElement>(null), composer = useRef<HTMLElement>(null);
	return <section ref={surface} className="relative flex h-screen min-h-0 flex-col bg-background text-foreground">
		<header className="flex h-12 shrink-0 items-center justify-between border-b px-4"><span className="text-xs text-muted-foreground">Decision fixture · no provider consent</span>
			<SessionConversationFrames sessionId={sessionId} sourceAgent="claude-code" activeBranchId="root" surfaceRef={surface} composerRef={composer} onLocateAttention={() => undefined} />
		</header>
		<article className="chat-scroll-viewport min-h-0 flex-1 overflow-auto p-6"><p className="mx-auto max-w-3xl">Conversation reading area. Dialogs stay above the composer.</p></article>
		<footer ref={composer} className="cursor-chat-composer-dock shrink-0 p-4"><textarea aria-label="Conversation draft" className="h-20 w-full rounded-xl border bg-card p-3" /></footer>
	</section>;
}
const element = document.getElementById("root");
if (!element) throw new Error("Missing fixture root");
createRoot(element).render(<QueryClientProvider client={client}><I18nextProvider i18n={i18n}><TooltipProvider><QuestionAnswerRuntime /><Harness /></TooltipProvider></I18nextProvider></QueryClientProvider>);
