import type { DecisionTarget } from "../../lib/decision-target";
import type { FrameRect } from "../../lib/conversation-frames";
import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useUiStore } from "../../stores/ui-store";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogOverlay, DialogTitle } from "../ui/dialog";
import { Button } from "../ui/button";
import { AttentionSourceUnavailable, AttentionUnavailable, attentionSourceQueryOptions, sessionAttentionQueryKey } from "../../hooks/useSessionAttention";
import { QuestionNotFound, QuestionsUnavailable, questionSourceQueryOptions, sessionQuestionQueryKey } from "../../hooks/useSessionQuestions";
import { InvalidQuestionResponse } from "../../lib/session-questions";
import { clientForSessionHost } from "../../lib/host-clients";
import { QuestionDecisionView } from "./QuestionDecisionView";

type Lookup = { target: DecisionTarget; scope: string } & (
	| { state: "loading" }
	| { state: "ready" }
	| { state: "error"; error: unknown }
);

export function DecisionDialog({ target, sessionId, hostId, sourceAgent, bounds, onClose, onLocate, onRestoreFocus }: {
	target?: DecisionTarget; sessionId: string; hostId?: string; sourceAgent?: string; bounds: { surface: HTMLElement; rect: FrameRect }; onClose: () => void; onLocate: (id: string) => void; onRestoreFocus: (resumeSheet?: boolean) => void;
}) {
	const { t } = useTranslation();
	const client = useQueryClient();
	const [lookup, setLookup] = useState<Lookup>();
	const nonce = useRef(0);
	const title = useRef<HTMLHeadingElement>(null);
	const closeRef = useRef(onClose); closeRef.current = onClose;
	const navigating = useRef(false);
	const fittedOut = useRef(false);
	const scope = JSON.stringify([hostId ?? null, sessionId]);
	const fits = Number.isFinite(bounds.rect.width) && Number.isFinite(bounds.rect.height) && bounds.rect.width >= 260 && bounds.rect.height >= 160;
	const load = useCallback((requested: DecisionTarget) => {
		const request = ++nonce.current;
		const currentScope = JSON.stringify([hostId ?? null, sessionId]);
		setLookup({ target: requested, scope: currentScope, state: "loading" });
		if (requested.kind === "worker_report") { setLookup({ target: requested, scope: currentScope, state: "ready" }); return; }
		const promise: Promise<unknown> = requested.kind === "agent_question"
			? client.fetchQuery({ ...questionSourceQueryOptions({ sessionId, hostId, questionId: requested.questionId }), staleTime: 0 })
			: client.fetchQuery({ ...attentionSourceQueryOptions({ sessionId, hostId, branchId: requested.branchId, activityId: requested.activityId }), staleTime: 0 });
		void promise.then(
			() => { if (nonce.current === request) setLookup({ target: requested, scope: currentScope, state: "ready" }); },
			(error: unknown) => { if (nonce.current === request) setLookup({ target: requested, scope: currentScope, state: "error", error }); },
		);
	}, [client, sessionId, hostId]);
	useEffect(() => {
		if (!target) { ++nonce.current; setLookup(undefined); return; }
		if (!fits) { fittedOut.current = true; ++nonce.current; setLookup(undefined); useUiStore.getState().showGlobalToast(t("decision.needSpace")); closeRef.current(); return; }
		fittedOut.current = false;
		load(target);
		return () => { ++nonce.current; };
	}, [target, fits, load, t]);
	const close = () => { ++nonce.current; setLookup(undefined); onClose(); };
	const ready = lookup && lookup.target === target && lookup.scope === scope && lookup.state === "ready";
	const error = lookup && lookup.target === target && lookup.scope === scope && lookup.state === "error" ? lookup.error : undefined;
	const box = bounds.surface.getBoundingClientRect();
	const compact = bounds.rect.height < 240;
	return <Dialog open={Boolean(target) && fits} onOpenChange={(open) => { if (!open) close(); }}>
		<DialogContent
			overlay={<DialogOverlay onClick={(event) => event.stopPropagation()} />}
			className="gap-0 overflow-hidden p-0"
			style={{ left: box.left + bounds.rect.left + bounds.rect.width / 2, top: box.top + bounds.rect.top + bounds.rect.height / 2, width: Math.min(560, bounds.rect.width), maxWidth: Math.min(560, bounds.rect.width), maxHeight: bounds.rect.height }}
			onOpenAutoFocus={(event) => { event.preventDefault(); title.current?.focus({ preventScroll: true }); }}
			onCloseAutoFocus={(event) => { event.preventDefault(); if (!navigating.current) onRestoreFocus(!fittedOut.current); navigating.current = false; fittedOut.current = false; }}
			onEscapeKeyDown={(event) => event.stopPropagation()}
			onClick={(event) => event.stopPropagation()}
		>
			<DialogHeader className={`shrink-0 border-b border-border pl-4 pr-10 ${compact ? "py-2" : "py-3"}`}>
				<DialogTitle ref={title} tabIndex={-1} className="rounded-sm text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring">{t("decision.title")}</DialogTitle>
				<DialogDescription className="sr-only">{t("decision.description")}</DialogDescription>
			</DialogHeader>
			{target && ready ? target.kind === "agent_question"
				? <QuestionSourceBody sessionId={sessionId} hostId={hostId} questionId={target.questionId} compact={compact} onClose={close} onReload={() => load(target)} />
				: target.kind === "worker_report"
					? <div
						className="flex min-h-0 flex-1 flex-col">
						<div
						className="min-h-0 flex-1 space-y-3 overflow-auto break-words p-4">
						<h3
						className="font-medium">{target.title}</h3>
						<p
						className="text-xs text-muted-foreground">{t("decision.source", { session: target.originSessionId, agent: target.originHarness })}</p>
						<p
						className="whitespace-pre-wrap text-sm">{target.explanation}</p>
						<p
						className="text-sm text-muted-foreground">{t("decision.readOnlyReport")}</p>
						</div>
						<DecisionClose
						onClose={close} />
						</div>
					: <ProviderSourceBody target={target}
						sessionId={sessionId} hostId={hostId}
						sourceAgent={sourceAgent}
						onClose={close}
						onReload={() => load(target)}
						onOriginal={() => { navigating.current = true; close(); requestAnimationFrame(() => onLocate(target.activityId)); }} />
				: <div className="flex min-h-0 flex-1 flex-col"><div className="min-h-0 flex-1 space-y-3 overflow-auto break-words p-4">
					{error ? <>
						<p role="alert"
						className="text-sm">{t(error instanceof QuestionsUnavailable || error instanceof AttentionUnavailable
							? "decision.unsupported"
							: error instanceof QuestionNotFound || error instanceof AttentionSourceUnavailable
							? "decision.unavailable"
							: error instanceof InvalidQuestionResponse
							? "decision.invalid"
							: "decision.loadError")}</p>
						<Button variant="outline"
						onClick={() => { if (target) load(target); }}>
					{t("decision.retry")}
				</Button>
						</> : <>
						<p
						className="text-sm font-medium">{target?.title}</p>
						<p role="status"
						className="text-sm text-muted-foreground">{t("decision.loading")}</p>
						</>}
				</div><DecisionClose onClose={close} /></div>}
		</DialogContent>
	</Dialog>;
}

function DecisionClose({ onClose }: { onClose: () => void }) {
	const { t } = useTranslation();
	return <div className="flex shrink-0 justify-end border-t border-border px-4 py-3"><Button variant="ghost" onClick={onClose}>{t("decision.later")}</Button></div>;
}

function QuestionSourceBody({ sessionId, hostId, questionId, compact, onClose, onReload }: { sessionId: string; hostId?: string; questionId: string; compact: boolean; onClose: () => void; onReload: () => void }) {
	const { t } = useTranslation();
	const client = useQueryClient();
	const query = useQuery(questionSourceQueryOptions({ sessionId, hostId, questionId }));
	const seen = useRef(new Set<number>());
	const [readError, setReadError] = useState(false);
	const question = query.data;
	const previousQuestion = useRef(question);
	useEffect(() => {
		const previous = previousQuestion.current;
		previousQuestion.current = question;
		if (question && previous?.id === question.id && previous.state === "open" && question.state !== "open") onClose();
	}, [question, onClose]);
	useEffect(() => {
		if (!question || query.isError || question.state !== "open" || question.read || seen.current.has(question.version)) return;
		seen.current.add(question.version);
		void Promise.resolve().then(() => clientForSessionHost(hostId).POST("/api/v1/sessions/{sessionId}/questions/{questionId}/read", { params: { path: { sessionId, questionId } }, body: { expectedVersion: question.version } }))
			.then(({ error }) => { if (error) setReadError(true); else { setReadError(false); void client.invalidateQueries({ queryKey: sessionQuestionQueryKey(sessionId, hostId) }, { cancelRefetch: false }); } }).catch(() => setReadError(true));
	}, [question, query.isError, client, sessionId, hostId, questionId]);
	if (!question) return <div className="p-4"><p role="alert">{t("decision.loadError")}</p><Button onClick={onReload}>{t("decision.retry")}</Button></div>;
	return <>{readError ? <p role="status"
		className="px-4 pt-2 text-xs text-muted-foreground">{t("decision.readError")}</p> : null}<QuestionDecisionView question={question} hostId={hostId} compact={compact}
		submitAllowed={!query.isError}
		onClose={onClose}
		onReload={onReload} />
		</>;
}

function ProviderSourceBody({ target, sessionId, hostId, sourceAgent, onClose, onReload, onOriginal }: {
	target: Extract<DecisionTarget, { kind: "provider_approval" | "provider_input" }>; sessionId: string; hostId?: string; sourceAgent?: string; onClose: () => void; onReload: () => void; onOriginal: () => void;
}) {
	const { t } = useTranslation();
	const client = useQueryClient();
	const query = useQuery(attentionSourceQueryOptions({ sessionId, hostId, branchId: target.branchId, activityId: target.activityId }));
	const item = query.data?.items.find((entry) => entry.activity.id === target.activityId);
	const matching = query.data?.conversationId === target.conversationId && query.data.activeBranchId === target.branchId && item?.branchId === target.sourceBranchId &&
		item.activity.requestId === target.requestId && item.activity.activityKind === (target.kind === "provider_approval" ? "approval" : "user_input");
	const actionable = Boolean(matching && item?.actionable && !query.isError);
	const seen = useRef(new Set<number>());
	const [readError, setReadError] = useState(false);
	useEffect(() => {
		if (!actionable || !item || item.read || seen.current.has(item.activity.revision)) return;
		seen.current.add(item.activity.revision);
		void Promise.resolve().then(() => clientForSessionHost(hostId).POST("/api/v1/sessions/{sessionId}/attention/{activityId}/read", { params: { path: { sessionId, activityId: target.activityId } }, body: { expectedBranchId: target.branchId, expectedRevision: item.activity.revision } }))
			.then(({ error }) => { if (error) setReadError(true); else { setReadError(false); void client.invalidateQueries({ queryKey: sessionAttentionQueryKey(sessionId, hostId) }, { cancelRefetch: false }); } }).catch(() => setReadError(true));
	}, [actionable, item, hostId, sessionId, target.activityId, target.branchId, client]);
	const message = item?.activity.detail?.message;
	const command = item?.activity.detail?.command;
	return <div className="flex min-h-0 flex-1 flex-col">
		<div className="min-h-0 flex-1 space-y-3 overflow-y-auto break-words px-4 py-3">
			<h3 className="whitespace-pre-wrap text-sm font-medium">{item?.activity.summary || target.title}</h3>
			<p className="text-xs text-muted-foreground">{t("decision.source", { session: sessionId, agent: sourceAgent ?? "agent" })}</p>
			<p className="text-xs text-muted-foreground">{t(target.kind === "provider_approval" ? "decision.providerApproval" : "decision.providerInput")}</p>
			{typeof message === "string" ? <p className="whitespace-pre-wrap text-sm">{message}</p> : null}
			{typeof command === "string" ? <pre className="max-w-full overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted p-3 font-mono text-xs">{command}</pre> : null}
			<p className="text-sm text-muted-foreground">{t("decision.providerSummary")}</p>
			{!actionable ? <p role="status" className="text-sm">{t(query.isError && !(query.error instanceof AttentionSourceUnavailable) ? "decision.loadError" : "decision.unavailable")}</p> : null}
			{readError ? <p role="status" className="text-xs text-muted-foreground">{t("decision.readError")}</p> : null}
		</div>
		<div className="flex shrink-0 flex-wrap justify-end gap-2 border-t border-border px-4 py-3">
			{query.isError ? <Button variant="outline" onClick={onReload}>{t("decision.retry")}</Button> : null}
			<Button variant="ghost" onClick={onClose}>{t("decision.later")}</Button>
			{actionable ? <Button onClick={onOriginal}>{t("decision.original")}</Button> : null}
		</div>
	</div>;
}
