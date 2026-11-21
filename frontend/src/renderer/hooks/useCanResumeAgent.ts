import {
	interfaceTransitionIsActive,
	type SessionInterfaceTransition,
	useSessionInterfaceTransitionStatus,
} from "./useSessionInterfaceTransition";
import { sessionAgentExited, sessionIsActive, type WorkspaceSession } from "../types/workspace";
import { apiErrorCode, apiErrorMessage } from "../lib/api-client";
import { appI18n } from "../i18n";
import { clientForSessionHost } from "../lib/host-clients";
import { followResume, noteResumeFailure, watchResume } from "../lib/resume-watch";

const resumesInFlight = new Map<string, Promise<void>>();
type ResumeIntent = "automatic" | "explicit";

export function resumeAgentErrorMessage(error: unknown, status?: number): string {
	switch (apiErrorCode(error)) {
		case "AGENT_NOT_EXITED": return appI18n.t("inspector.resumeAlreadyRunning");
		case "AGENT_RESUME_IN_PROGRESS": return appI18n.t("inspector.resumeInProgress");
		case "CHAT_RESUME_FAILED": return appI18n.t("inspector.resumeSavedConversationFailed");
		case "CHAT_AUTH_REQUIRED":
		case "AGENT_AUTH_REQUIRED": return appI18n.t("inspector.resumeAuthRequired");
		default: return apiErrorMessage(error, `Failed to resume agent (${status ?? "unknown"})`);
	}
}

/** A failed resume as a thrown Error that keeps the API error code. */
export function resumeAgentError(error: unknown, status?: number): Error {
	return Object.assign(new Error(resumeAgentErrorMessage(error, status)), { code: apiErrorCode(error) });
}

/** A failed resume says another app still holds the chat. */
export function resumeOpenElsewhere(error: unknown): boolean {
	return apiErrorCode(error) === "SESSION_OPEN_ELSEWHERE";
}

/**
 * Ask the daemon to resume a local session's stopped agent, sharing one
 * request per session and intent. The daemon answers once the resume starts; the resume
 * watch follows it from there and runs refresh when it settles. A resume
 * already in progress is followed the same way, not reported as a failure.
 */
export function resumeLocalAgent(sessionId: string, refresh: () => unknown, intent: ResumeIntent): Promise<void> {
	return resumeOnce(sessionId, async () => {
		const { data, error, response } = await clientForSessionHost().POST("/api/v1/sessions/{sessionId}/resume-agent", {
			params: { path: { sessionId }, ...(intent === "automatic" && { query: { automatic: true } }) },
		});
		if (!error) {
			followResume(sessionId, undefined, response, data, refresh);
			return;
		}
		if (apiErrorCode(error) === "AGENT_RESUME_IN_PROGRESS") {
			watchResume(sessionId, undefined, refresh);
			return;
		}
		noteResumeFailure(sessionId, error);
		throw resumeAgentError(error, response.status);
	}, intent);
}

/** Later callers with the same intent join the request in flight. */
export function resumeOnce(sessionId: string, run: () => Promise<void>, intent: ResumeIntent = "explicit"): Promise<void> {
	const key = `${intent}:${sessionId}`;
	const existing = resumesInFlight.get(key);
	if (existing) return existing;
	const request = run().finally(() => resumesInFlight.delete(key));
	resumesInFlight.set(key, request);
	return request;
}

export function canAutoResumeAgent(
	session: WorkspaceSession | undefined,
	transition?: SessionInterfaceTransition,
): boolean {
	return !session?.chatExplicitlyExited && canResumeAgent(session, transition);
}

export function canResumeAgent(
	session: WorkspaceSession | undefined,
	transition?: SessionInterfaceTransition,
): boolean {
	return Boolean(
		session &&
			(session.needsResume && sessionIsActive(session) || sessionAgentExited(session)) &&
			!session.cloud &&
			!interfaceTransitionIsActive(transition),
	);
}

export function useCanResumeAgent(session: WorkspaceSession | undefined, hostId?: string): boolean {
	const baseEligible = canResumeAgent(session);
	const interfaceTransition = useSessionInterfaceTransitionStatus(baseEligible ? session?.id : undefined, hostId);
	return Boolean(
		baseEligible &&
			!interfaceTransition.isLoading &&
			!interfaceTransition.statusError &&
			!interfaceTransitionIsActive(interfaceTransition.transition),
	);
}
