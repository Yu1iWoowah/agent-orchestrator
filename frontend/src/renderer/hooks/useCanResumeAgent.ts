import {
	interfaceTransitionIsActive,
	type SessionInterfaceTransition,
	useSessionInterfaceTransitionStatus,
} from "./useSessionInterfaceTransition";
import { sessionAgentExited, type WorkspaceSession } from "../types/workspace";
import { apiErrorCode, apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import { followResume, noteResumeFailure, watchResume } from "../lib/resume-watch";

const resumesInFlight = new Map<string, Promise<void>>();

/**
 * Ask the daemon to resume a local session's stopped agent, sharing one
 * request per session. The daemon answers once the resume starts; the resume
 * watch follows it from there and runs refresh when it settles. A resume
 * already in progress is followed the same way, not reported as a failure.
 */
export function resumeAgentOnOpen(sessionId: string, refresh: () => unknown): Promise<void> {
	return resumeOnce(sessionId, async () => {
		const { data, error, response } = await clientForSessionHost().POST("/api/v1/sessions/{sessionId}/resume-agent", {
			params: { path: { sessionId } },
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
		throw new Error(apiErrorMessage(error, `Failed to resume agent (${response.status})`));
	});
}

/** Run one resume per local session; later callers join the request in flight. */
export function resumeOnce(sessionId: string, run: () => Promise<void>): Promise<void> {
	const existing = resumesInFlight.get(sessionId);
	if (existing) return existing;
	const request = run().finally(() => resumesInFlight.delete(sessionId));
	resumesInFlight.set(sessionId, request);
	return request;
}

export function canResumeAgent(
	session: WorkspaceSession | undefined,
	transition?: SessionInterfaceTransition,
): boolean {
	return Boolean(
		session &&
			sessionAgentExited(session) &&
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
