import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { questionHost, type DecisionTarget, type QuestionHost, type QuestionRef } from "../lib/decision-target";
import { LOCAL_HOST } from "../lib/hosts";
import { orchestratorNavigateTarget, projectNavigateTarget, questionNavigateTarget, sessionNavigateTarget } from "../lib/navigate-to-session";
import type { WorkspaceSummary } from "../types/workspace";
import type { FrameRect } from "../lib/conversation-frames";
import { DecisionDialog } from "./chat/DecisionDialog";

type Routing = {
	hostFor: (projectId: string, hostId?: string) => QuestionHost;
	openQuestion: (ref: QuestionRef, restoreFocus?: () => void) => void;
	openOriginal: (projectId: string, sessionId: string, hostId?: string) => void;
};
const Context = createContext<Routing | undefined>(undefined);
export const useProjectQuestionRouting = () => useContext(Context);

export function ProjectQuestionRouting({ children, workspaces, remoteWorkspaces, localReady, loadedProjectHostIds, loadedSessionHostIds, failedHostIds, onRetry }: {
	children: ReactNode; workspaces: WorkspaceSummary[]; remoteWorkspaces: WorkspaceSummary[]; localReady: boolean;
	loadedProjectHostIds: string[]; loadedSessionHostIds: string[]; failedHostIds: string[]; onRetry: () => void;
}) {
	const { t } = useTranslation();
	const navigate = useNavigate();
	const params = useParams({ strict: false });
	const { question } = useSearch({ strict: false });
	const returnTo = useRef<{ scope: string; restore?: () => void } | undefined>(undefined);
	const [bounds, setBounds] = useState<{ surface: HTMLElement; rect: FrameRect }>();
	const hostFor = useCallback((projectId: string, hostId = LOCAL_HOST) => {
		const local = hostId === LOCAL_HOST;
		const workspace = (local ? workspaces : remoteWorkspaces).find((entry) => entry.id === projectId && (entry.hostId ?? LOCAL_HOST) === hostId);
		const ready = local ? localReady : loadedProjectHostIds.includes(hostId) && loadedSessionHostIds.includes(hostId) && !failedHostIds.includes(hostId);
		return questionHost({ workspace, ready, projectId, hostId });
	}, [workspaces, remoteWorkspaces, localReady, loadedProjectHostIds, loadedSessionHostIds, failedHostIds]);
	const openQuestion = useCallback((ref: QuestionRef, restore?: () => void) => {
		returnTo.current = { scope: JSON.stringify([ref.hostId, ref.projectId]), restore };
		const host = hostFor(ref.projectId, ref.hostId);
		void navigate(questionNavigateTarget(ref, host.kind === "orchestrator" ? host.sessionId : undefined));
	}, [hostFor, navigate]);
	const openOriginal = useCallback((projectId: string, sessionId: string, hostId?: string) => {
		void navigate({ ...sessionNavigateTarget(projectId, sessionId, hostId), search: {} });
	}, [navigate]);
	const matchesScope = Boolean(question && params.projectId === question.projectId && (params.hostId ?? LOCAL_HOST) === question.hostId);
	const host = question ? hostFor(question.projectId, question.hostId) : { kind: "unknown" as const };
	const orchestratorId = host.kind === "orchestrator" ? host.sessionId : undefined;
	useEffect(() => {
		if (!question || !matchesScope || host.kind === "unknown") return;
		if ((host.kind === "orchestrator" && params.sessionId !== orchestratorId) || (host.kind === "none" && params.sessionId))
			void navigate({ ...questionNavigateTarget(question, orchestratorId), replace: true });
	}, [question, matchesScope, host.kind, orchestratorId, params.sessionId, navigate]);
	const visible = matchesScope ? question : undefined;
	useLayoutEffect(() => {
		if (!visible) { setBounds(undefined); return; }
		const surface = document.querySelector<HTMLElement>(".center-panel-surface") ?? document.documentElement;
		const measure = () => {
			const box = surface.getBoundingClientRect();
			setBounds({ surface, rect: { left: 16, top: 16, width: Math.max(0, box.width - 32), height: Math.max(0, box.height - 32) } });
		};
		measure();
		const observer = new ResizeObserver(measure); observer.observe(surface);
		window.addEventListener("resize", measure);
		return () => { observer.disconnect(); window.removeEventListener("resize", measure); };
	}, [visible?.hostId, visible?.projectId, visible?.originSessionId, visible?.questionId, params.sessionId]);
	const restoreFocus = () => {
		const saved = returnTo.current; returnTo.current = undefined;
		if (saved?.scope === JSON.stringify([params.hostId ?? LOCAL_HOST, params.projectId])) saved.restore?.();
	};
	const close = () => {
		if (question) void navigate(params.sessionId
			? { ...orchestratorNavigateTarget(question.projectId, params.sessionId, question.hostId), replace: true }
			: { ...projectNavigateTarget(question.projectId, question.hostId), search: {}, replace: true });
		restoreFocus();
	};
	const target = useMemo<DecisionTarget | undefined>(() => visible ? { kind: "agent_question", questionId: visible.questionId, title: t("decision.agentQuestion") } : undefined,
		[visible?.hostId, visible?.projectId, visible?.originSessionId, visible?.questionId, t]);
	const editable = host.kind === "none" ? !params.sessionId : host.kind === "orchestrator" && params.sessionId === host.sessionId;
	return <Context.Provider value={{ hostFor, openQuestion, openOriginal }}>
		{children}
		{visible && bounds ? <DecisionDialog target={target} sessionId={visible.originSessionId} hostId={visible.hostId === LOCAL_HOST ? undefined : visible.hostId}
			projectId={visible.projectId} editable={editable} routingPending={host.kind === "unknown"} onRoutingRetry={onRetry}
			bounds={bounds} onClose={close} onRestoreFocus={restoreFocus} onLocate={() => {}} /> : null}
	</Context.Provider>;
}
