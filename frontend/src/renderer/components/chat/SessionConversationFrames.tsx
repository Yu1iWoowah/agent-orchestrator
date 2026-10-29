import { useTranslation } from "react-i18next";
import { type RefObject, useCallback, useEffect, useRef, useState } from "react";
import { Check, Circle, LoaderCircle } from "lucide-react";
import { useConversationFrameSafeRect } from "../../hooks/useConversationFrameSafeRect";
import {
	PlanUnavailable,
	useSessionPlan,
	useSessionPlanHistory,
	type SessionPlanVersion,
} from "../../hooks/useSessionPlan";
import { ConversationFrames } from "./ConversationFrames";
import { Button } from "../ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import { useSessionAttentionFrame } from "./SessionAttentionContent";
import { useAttentionSelection } from "../../hooks/useSessionAttention";
import type { ConversationActivity } from "../../types/conversation";
import { DecisionDialog } from "./DecisionDialog";
import { decisionTargetKey, type DecisionTarget } from "../../lib/decision-target";
import { conversationFrameSessionKey } from "../../stores/conversation-frame-store";

export interface AttentionSourceSelection {
	scope: string;
	selectedId?: string;
	activity?: ConversationActivity;
	loading: boolean;
	error: boolean;
	unavailable: boolean;
	readError: boolean;
}

export function SessionConversationFrames({
	sessionId,
	hostId,
	sourceAgent,
	surfaceRef,
	composerRef,
	activeBranchId,
	onLocateAttention,
	onAttentionSourceChange,
	onAttentionSourceFocus,
}: {
	sessionId: string;
	hostId?: string;
	sourceAgent?: string;
	surfaceRef: RefObject<HTMLElement | null>;
	composerRef: RefObject<HTMLElement | null>;
	activeBranchId?: string;
	onLocateAttention?: (id: string) => void;
	onAttentionSourceChange?: (selection: AttentionSourceSelection) => void;
	onAttentionSourceFocus?: (activity: ConversationActivity) => boolean;
}) {
	const { t } = useTranslation();
	const bounds = useConversationFrameSafeRect(surfaceRef, composerRef);
	const plan = useSessionPlan(sessionId, hostId);
	const selection = useAttentionSelection(
		sessionId,
		hostId,
		activeBranchId,
		Boolean(onAttentionSourceChange),
	);
	const scope = JSON.stringify([hostId ?? null, sessionId, activeBranchId ?? null]);
	const currentScope = useRef(scope); currentScope.current = scope;
	const [decision, setDecision] = useState<{ scope: string; target: DecisionTarget }>();
	const returnTo = useRef<{ scope: string; trigger: HTMLButtonElement; targetKey: string; resume?: () => void } | undefined>(undefined);
	useEffect(() => { setDecision(undefined); returnTo.current = undefined; }, [scope]);
	const openDecision = useCallback((target: DecisionTarget, trigger: HTMLButtonElement, resume?: () => void) => {
		returnTo.current = { scope, trigger, targetKey: decisionTargetKey(target), resume };
		setDecision({ scope, target });
	}, [scope]);
	const restoreFocus = useCallback((resumeSheet = true) => {
		const saved = returnTo.current;
		if (!saved || saved.scope !== currentScope.current) return;
		if (resumeSheet) saved.resume?.();
		requestAnimationFrame(() => {
			if (saved.scope !== currentScope.current) return;
			const visible = (node: HTMLElement) => node.isConnected && !node.closest("[hidden]") && !node.closest('[data-slot="sheet-content"][data-state="closed"]');
			const row = visible(saved.trigger) ? saved.trigger : Array.from(document.querySelectorAll<HTMLButtonElement>("[data-decision-target]")).find((node) => node.dataset.decisionTarget === saved.targetKey && node.dataset.decisionScope === saved.scope && visible(node));
			const frameKey = conversationFrameSessionKey(sessionId, hostId);
			const entry = Array.from(document.querySelectorAll<HTMLButtonElement>('[data-conversation-frame-entry="attention"]')).find((node) => node.dataset.conversationFrameScope === frameKey);
			(row ?? entry)?.focus({ preventScroll: true });
		});
	}, [sessionId, hostId]);
	const locateOriginal = useCallback((id: string) => {
		if (scope !== currentScope.current) return;
		returnTo.current = undefined;
		onLocateAttention?.(id);
		if (onAttentionSourceChange) selection.locate(id);
	}, [scope, onLocateAttention, onAttentionSourceChange, selection.locate]);
	const attention = useSessionAttentionFrame({
		sessionId,
		hostId,
		branchId: activeBranchId,
		onOpen: openDecision,
		locatingId: selection.loading ? selection.selectedId : undefined,
	});
	const focusedSelection = useRef<number | undefined>(undefined);
	useEffect(() => {
		onAttentionSourceChange?.({
			scope: selection.scope,
			selectedId: selection.selectedId,
			activity: selection.activity,
			loading: selection.loading,
			error: selection.error,
			unavailable: selection.unavailable,
			readError: selection.readError,
		});
	}, [
		onAttentionSourceChange,
		selection.scope,
		selection.selectedId,
		selection.activity,
		selection.loading,
		selection.error,
		selection.unavailable,
		selection.readError,
	]);
	useEffect(() => {
		if (
			!selection.activity ||
			selection.selectionNonce === focusedSelection.current
		)
			return;
		const frame = requestAnimationFrame(() => {
			if (selection.activity && onAttentionSourceFocus?.(selection.activity)) {
				focusedSelection.current = selection.selectionNonce;
				selection.markViewed();
			}
		});
		return () => cancelAnimationFrame(frame);
	}, [selection.activity, selection.selectionNonce, onAttentionSourceFocus]);
	const [tab, setTab] = useState("latest");
	const history = useSessionPlanHistory(
		sessionId,
		hostId,
		plan.data?.activeBranchId,
		tab === "history",
	);
	const steps = plan.data?.version?.plan.steps ?? [];
	const summary = plan.isError
		? "?"
		: plan.isPending
			? "…"
			: plan.data?.state === "cleared"
				? t("plan.clearedShort")
				: steps.length
					? `${steps.filter((step) => step.status === "completed").length}/${steps.length}`
					: t("plan.absentShort");
	if (!bounds) return null;
	const content = (
		<Tabs value={tab} onValueChange={setTab}>
			<TabsList className="mb-3 h-8">
				<TabsTrigger value="latest">{t("plan.latest")}</TabsTrigger>
				<TabsTrigger value="history">{t("plan.history")}</TabsTrigger>
			</TabsList>
			<TabsContent value="latest">
				{plan.isError ? (
					<div role="alert" className="text-xs text-muted-foreground">
						<p>
							{plan.error instanceof PlanUnavailable
								? t("plan.unsupported")
								: t("plan.offline")}
						</p>
						{plan.error instanceof PlanUnavailable ? null : (
							<Button
								variant="ghost"
								size="sm"
								onClick={() => void plan.refetch()}
							>
								{t("plan.retry")}
							</Button>
						)}
					</div>
				) : plan.isPending ? (
					<p role="status" className="text-xs text-muted-foreground">
						{t("plan.loading")}
					</p>
				) : plan.data?.state === "absent" ? (
					<p className="text-xs text-muted-foreground">{t("plan.noPlan")}</p>
				) : plan.data?.version ? (
					<PlanVersion version={plan.data.version} />
				) : null}
			</TabsContent>
			<TabsContent value="history">
				{history.isError ? (
					<div role="alert">
						<p className="text-xs text-muted-foreground">
							{t("plan.historyError")}
						</p>
						<Button
							variant="ghost"
							size="sm"
							onClick={() => void history.refetch()}
						>
							{t("plan.retry")}
						</Button>
					</div>
				) : history.isPending ? (
					<p role="status" className="text-xs text-muted-foreground">
						{t("plan.historyLoading")}
					</p>
				) : (
					history.data?.pages
						.flatMap((page) => page.versions)
						.map((version) => (
							<details
								key={`${version.branchId}-${version.sequence}`}
								className="mb-3 border-b pb-2"
							>
								<summary className="cursor-pointer text-xs text-muted-foreground">
									{new Date(version.updatedAt).toLocaleString()} ·{" "}
									{version.turnId || t("plan.unknownTurn")}
								</summary>
								<div className="pt-2">
									<PlanVersion version={version} />
								</div>
							</details>
						))
				)}
				{history.data?.pages.every((page) => page.versions.length === 0) ? (
					<p className="text-xs text-muted-foreground">{t("plan.noHistory")}</p>
				) : null}
				{history.hasNextPage ? (
					<Button
						variant="ghost"
						size="sm"
						disabled={history.isFetchingNextPage}
						onClick={() => void history.fetchNextPage()}
					>
						{t("plan.older")}
					</Button>
				) : null}
			</TabsContent>
		</Tabs>
	);
	return <>
		<ConversationFrames
			key={`${hostId ?? ""}:${sessionId}`}
			sessionId={sessionId}
			hostId={hostId}
			surface={bounds.surface}
			safeRect={bounds.rect}
			frames={[
				attention,
				{ kind: "plan", title: t("plan.title"), summary, content },
			]}
		/>
		<DecisionDialog target={decision?.scope === scope ? decision.target : undefined} sessionId={sessionId} hostId={hostId} sourceAgent={sourceAgent} bounds={bounds} onClose={() => setDecision(undefined)} onLocate={locateOriginal} onRestoreFocus={restoreFocus} />
	</>;
}

function PlanVersion({ version }: { version: SessionPlanVersion }) {
	const { t } = useTranslation();
	return (
		<div>
			<p className="mb-3 text-xs text-muted-foreground">
				<time dateTime={version.updatedAt}>
					{new Date(version.updatedAt).toLocaleString()}
				</time>{" "}
				·{" "}
				{version.turnId
					? t("plan.turn", { id: version.turnId })
					: t("plan.unknownTurn")}
			</p>
			{version.plan.explanation ? (
				<p className="mb-3 whitespace-pre-wrap text-sm text-muted-foreground">
					{version.plan.explanation}
				</p>
			) : null}
			{version.plan.steps.length ? (
				<ol className="space-y-2">
					{version.plan.steps.map((step, index) => (
						<li
							key={`${index}-${step.text}`}
							className="flex gap-2 text-sm"
							aria-label={`${step.text} — ${step.status}`}
						>
							{step.status === "completed" ? (
								<Check
									aria-hidden="true"
									className="mt-0.5 size-4 shrink-0 text-muted-foreground"
								/>
							) : step.status === "in_progress" ? (
								<LoaderCircle
									aria-hidden="true"
									className="mt-0.5 size-4 shrink-0 text-primary"
								/>
							) : (
								<Circle
									aria-hidden="true"
									className="mt-0.5 size-4 shrink-0 text-muted-foreground"
								/>
							)}
							<span
								className={
									step.status === "completed"
										? "text-muted-foreground line-through"
										: "text-foreground"
								}
							>
								{step.text}
							</span>
						</li>
					))}
				</ol>
			) : (
				<p className="text-xs text-muted-foreground">{t("plan.cleared")}</p>
			)}
		</div>
	);
}
