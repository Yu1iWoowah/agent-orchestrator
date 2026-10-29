import { useState } from "react";
import { useTranslation } from "react-i18next";
import { AttentionUnavailable, type AttentionItem, useSessionAttention } from "../../hooks/useSessionAttention";
import { QuestionsUnavailable, useSessionQuestions } from "../../hooks/useSessionQuestions";
import { InvalidQuestionResponse, type SessionQuestion } from "../../lib/session-questions";
import { decisionTargetKey, type DecisionTarget } from "../../lib/decision-target";
import { Button } from "../ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import { useConversationFrameDialog, type ConversationFrameDescriptor } from "./ConversationFrames";

type AttentionRowSource =
	| { kind: "agent_question"; question: SessionQuestion }
	| { kind: "provider"; item: AttentionItem; conversationId: string; branchId: string };

export function useSessionAttentionFrame({ sessionId, hostId, branchId, onOpen, locatingId }: {
	sessionId: string; hostId?: string; branchId?: string;
	onOpen: (target: DecisionTarget, trigger: HTMLButtonElement, resume?: () => void) => void;
	locatingId?: string;
}): ConversationFrameDescriptor {
	const { t } = useTranslation();
	const [tab, setTab] = useState("open");
	const open = useSessionAttention(sessionId, hostId, branchId, "open");
	const history = useSessionAttention(sessionId, hostId, branchId, "history", tab === "history");
	const questionOpen = useSessionQuestions(sessionId, hostId, "open");
	const questionHistory = useSessionQuestions(sessionId, hostId, "history", tab === "history");
	const provider = tab === "history" ? history : open;
	const questions = tab === "history" ? questionHistory : questionOpen;
	const rows = new Map<string, AttentionRowSource>();
	for (const page of provider.data?.pages ?? []) for (const item of page.items)
		rows.set(`provider:${item.activity.id}`, { kind: "provider", item, conversationId: page.conversationId, branchId: page.activeBranchId });
	for (const page of questions.data?.pages ?? []) for (const question of page.items)
		rows.set(`question:${question.id}`, { kind: "agent_question", question });
	const ordered = [...rows.entries()].sort(([, a], [, b]) => {
		const at = a.kind === "agent_question" ? a.question.createdAt : a.item.activity.createdAt;
		const bt = b.kind === "agent_question" ? b.question.createdAt : b.item.activity.createdAt;
		return Date.parse(bt) - Date.parse(at);
	});
	const known = (open.isSuccess ? open.data.pages[0]?.counts.open ?? 0 : 0) + (questionOpen.isSuccess ? questionOpen.data.pages[0]?.open ?? 0 : 0);
	const unknown = open.isError || questionOpen.isError || open.isPending || questionOpen.isPending;
	const summary = open.isPending && questionOpen.isPending ? "…" : unknown ? known ? `${known}+?` : "?" : String(known);
	return {
		kind: "attention", title: t("attention.title"), summary,
		content: <Tabs value={tab} onValueChange={setTab}>
			<TabsList className="mb-3 h-8"><TabsTrigger value="open">{t("attention.pending")}</TabsTrigger><TabsTrigger value="history">{t("attention.history")}</TabsTrigger></TabsList>
			{(["open", "history"] as const).map((value) => <TabsContent key={value} value={value}>
				{provider.isError ? <div role="alert" className="mb-3 text-xs text-muted-foreground"><p>{t(provider.error instanceof AttentionUnavailable ? "attention.unsupported" : "attention.offline")}</p>{!(provider.error instanceof AttentionUnavailable) ? <Button variant="ghost" size="sm" onClick={() => void provider.refetch()}>{t("attention.retry")}</Button> : null}</div> : null}
				{questions.isError ? <div role="alert" className="mb-3 text-xs text-muted-foreground"><p>{t(questions.error instanceof QuestionsUnavailable ? "decision.unsupported" : questions.error instanceof InvalidQuestionResponse ? "decision.invalid" : "decision.loadError")}</p>{!(questions.error instanceof QuestionsUnavailable) ? <Button variant="ghost" size="sm" onClick={() => void questions.refetch()}>{t("decision.retry")}</Button> : null}</div> : null}
				{provider.isPending || questions.isPending ? <p role="status" className="mb-3 text-xs text-muted-foreground">{t("attention.loading")}</p> : null}
				{ordered.length ? <ul className="divide-y divide-border">{ordered.map(([key, source]) => <AttentionRow key={key} source={source} sessionId={sessionId} hostId={hostId} scopeBranch={branchId} onOpen={onOpen} locating={source.kind === "provider" && locatingId === source.item.activity.id} />)}</ul> : provider.isSuccess && questions.isSuccess ? <p className="text-xs text-muted-foreground">{t(value === "open" ? "attention.empty" : "attention.noHistory")}</p> : null}
				{provider.hasNextPage ? <Button variant="ghost" size="sm" disabled={provider.isFetchingNextPage} onClick={() => void provider.fetchNextPage()}>{t("attention.older")}</Button> : null}
				{questions.hasNextPage ? <Button variant="ghost" size="sm" disabled={questions.isFetchingNextPage} onClick={() => void questions.fetchNextPage()}>{t("decision.older")}</Button> : null}
			</TabsContent>)}
		</Tabs>,
	};
}

function AttentionRow({ source, sessionId, hostId, scopeBranch, onOpen, locating }: {
	source: AttentionRowSource; sessionId: string; hostId?: string; scopeBranch?: string;
	onOpen: (target: DecisionTarget, trigger: HTMLButtonElement, resume?: () => void) => void; locating: boolean;
}) {
	const { t } = useTranslation();
	const beginDialog = useConversationFrameDialog();
	let target: DecisionTarget | undefined;
	let title: string;
	let pending: boolean;
	let unread: boolean;
	let status: "attention.answered" | "attention.cancelled" | "attention.unavailable" | "decision.expired" = "attention.unavailable";
	if (source.kind === "agent_question") {
		const question = source.question;
		title = question.payload.title; pending = question.state === "open"; unread = pending && !question.read;
		status = question.state === "answered" ? "attention.answered" : question.state === "cancelled" ? "attention.cancelled" : "decision.expired";
		target = { kind: "agent_question", questionId: question.id, title };
	} else {
		const item = source.item;
		const message = item.activity.detail?.message;
		title = typeof message === "string" && message.trim() ? message : item.activity.summary || t("attention.request");
		pending = item.actionable; unread = pending && !item.read;
		status = item.activity.status === "resolved" || item.activity.status === "completed" ? "attention.answered" : item.activity.status === "cancelled" ? "attention.cancelled" : "attention.unavailable";
		if (item.activity.activityKind === "approval" || item.activity.activityKind === "user_input") target = {
			kind: item.activity.activityKind === "approval" ? "provider_approval" : "provider_input", title,
			activityId: item.activity.id, requestId: item.activity.requestId ?? "", conversationId: source.conversationId, branchId: source.branchId, sourceBranchId: item.branchId,
		};
	}
	return <li className="min-w-0 py-3 first:pt-0"><button type="button" className="flex w-full min-w-0 flex-col gap-1 rounded-md p-1 text-left outline-none hover:bg-muted/60 focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-wait"
		aria-label={t("decision.open", { title })} disabled={!target || locating}
		data-decision-target={target ? decisionTargetKey(target) : undefined} data-decision-scope={JSON.stringify([hostId ?? null, sessionId, scopeBranch ?? null])}
		onClick={(event) => { if (target) { const resume = beginDialog?.(); onOpen(target, event.currentTarget, resume); } }}>
		<span className="flex flex-wrap gap-x-2 gap-y-1 text-xs text-muted-foreground">
			<span>{t(source.kind === "agent_question" ? source.question.kind === "authorization" ? "decision.authorization" : "decision.agentQuestion" : source.item.activity.activityKind === "approval" ? "attention.approval" : "attention.input")}</span>
			<span>{source.kind === "agent_question" ? `${source.question.originSessionId} · ${source.question.originHarness}` : t("attention.thisSession")}</span>
			{source.kind === "agent_question" ? <span>{t("decision.questionCount", { count: source.question.payload.questions.length })}</span> : null}
			{unread ? <span className="text-status-needs-you">{t("attention.unread")}</span> : null}
		</span>
		<span className="whitespace-pre-wrap break-words text-sm text-foreground">{title}</span>
		{!pending ? <span className="text-xs text-muted-foreground">{t(status)}</span> : null}
	</button></li>;
}
