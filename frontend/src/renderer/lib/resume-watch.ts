import { useSyncExternalStore } from "react";
import { apiErrorCode, apiErrorMessage } from "./api-client";
import { aoBridge } from "./bridge";
import { clientForSessionHost } from "./host-clients";
import { appI18n } from "../i18n";

/**
 * The daemon answers resume-agent once the resume starts and finishes it in the
 * background. This app-level store follows each resume to its end, keyed by
 * host and session, so the outcome is recorded even after the screen that
 * started it is gone. A failed resume stays recorded until the agent is seen
 * running or a later resume succeeds; opening the session meanwhile does not
 * resume it again, only the Resume button does.
 */
export type ResumeWatch = { state: "running" } | { state: "failed"; message: string };

/** Spacing of the status reads while a resume runs. */
export const RESUME_WATCH_INTERVAL_MS = 1_000;
/** The watch gives up after this long, like the CLI. */
export const RESUME_WATCH_LIMIT_MS = 150_000;
/** Opening a session waits this long for the status before resuming anyway. */
export const RESUME_CHECK_LIMIT_MS = 10_000;

const watches = new Map<string, ResumeWatch>();
const listeners = new Set<() => void>();
/**
 * Sessions seen running since this renderer last started or recorded a
 * resume. The daemon keeps only the latest resume-agent request, so an agent
 * started another way (a wake, a version move) leaves its old failure there.
 */
const seenRunning = new Set<string>();

function watchKey(sessionId: string, hostId?: string): string {
	return `${hostId ?? "local"}\u0000${sessionId}`;
}

function update(key: string, watch: ResumeWatch | undefined): void {
	if (watch) watches.set(key, watch);
	else watches.delete(key);
	for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
	listeners.add(listener);
	return () => listeners.delete(listener);
}

/** The resume being followed for this session, or its recorded failure. */
export function useResumeWatch(sessionId: string | undefined, hostId?: string): ResumeWatch | undefined {
	return useSyncExternalStore(subscribe, () => (sessionId ? watches.get(watchKey(sessionId, hostId)) : undefined));
}

/** Whether the session's latest resume failed and no running agent was seen since. */
export function resumeFailed(sessionId: string, hostId?: string): boolean {
	return watches.get(watchKey(sessionId, hostId))?.state === "failed";
}

/** Record a failed resume. A resume already in progress is not a failure. */
export function noteResumeFailure(sessionId: string, error: unknown, hostId?: string): void {
	if (apiErrorCode(error) === "AGENT_RESUME_IN_PROGRESS") return;
	const key = watchKey(sessionId, hostId);
	seenRunning.delete(key);
	update(key, { state: "failed", message: apiErrorMessage(error, "Failed to resume agent") });
}

/**
 * Forget a failed resume once the agent is seen running, including the one the
 * daemon still reports for its latest resume request.
 */
export function clearResumeFailure(sessionId: string, hostId?: string): void {
	const key = watchKey(sessionId, hostId);
	seenRunning.add(key);
	if (watches.get(key)?.state === "failed") update(key, undefined);
}

function notifySavedPrompt(sessionId: string, hostId?: string): void {
	void aoBridge.notifications
		.show({
			id: `resume-agent-fallback:${hostId ?? "local"}:${sessionId}:${Date.now()}`,
			title: appI18n.t("inspector.startedFromPrompt"),
			body: appI18n.t("inspector.resumeFallbackBody"),
		})
		.catch((err) => console.warn("Unable to show resume fallback notification", err));
}

/**
 * Follow a resume the daemon accepted. A 202 is read until it settles; an
 * older host that answered 200 already resumed inline and carries resumeMode.
 * refresh runs once the outcome is known.
 */
export function followResume(
	sessionId: string,
	hostId: string | undefined,
	response: { status: number },
	data: unknown,
	refresh: () => unknown,
): void {
	if (response.status === 202) {
		watchResume(sessionId, hostId, refresh);
		return;
	}
	clearResumeFailure(sessionId, hostId);
	if ((data as { resumeMode?: string } | undefined)?.resumeMode === "saved_prompt") notifySavedPrompt(sessionId, hostId);
}

/** Read the session's resume status until it settles; one watch per session. */
export function watchResume(sessionId: string, hostId: string | undefined, refresh: () => unknown): void {
	const key = watchKey(sessionId, hostId);
	if (watches.get(key)?.state === "running") return;
	seenRunning.delete(key);
	const running: ResumeWatch = { state: "running" };
	update(key, running);
	void follow(key, running, sessionId, hostId, refresh);
}

async function follow(key: string, running: ResumeWatch, sessionId: string, hostId: string | undefined, refresh: () => unknown) {
	const deadline = Date.now() + RESUME_WATCH_LIMIT_MS;
	const finish = (watch: ResumeWatch | undefined) => {
		if (watches.get(key) !== running) return;
		update(key, watch);
		void refresh();
	};
	for (;;) {
		await new Promise((resolve) => setTimeout(resolve, RESUME_WATCH_INTERVAL_MS));
		if (watches.get(key) !== running) return;
		if (Date.now() > deadline) {
			finish(undefined);
			return;
		}
		// A read still pending at the deadline is abandoned with the watch.
		const status = await readResume(sessionId, hostId, deadline - Date.now());
		if (watches.get(key) !== running) return;
		if (status === "retry") {
			if (Date.now() >= deadline) {
				finish(undefined);
				return;
			}
			continue;
		}
		if (status === "gone") {
			finish(undefined);
			return;
		}
		if (status.state === "succeeded") {
			finish(undefined);
			if (status.resumeMode === "saved_prompt") notifySavedPrompt(sessionId, hostId);
			return;
		}
		if (status.state === "failed") {
			finish(apiErrorCode(status.error) === "AGENT_RESUME_IN_PROGRESS"
				? undefined
				: { state: "failed", message: status.error?.message || "Failed to resume agent" });
			return;
		}
	}
}

type ResumeStatus = { state: string; resumeMode?: string; error?: { code: string; message: string } };

/**
 * One status read, abandoned after limitMs: "gone" when the host has no
 * record, "retry" on a passing failure or when it took too long.
 */
async function readResume(sessionId: string, hostId: string | undefined, limitMs: number): Promise<ResumeStatus | "gone" | "retry"> {
	const abort = new AbortController();
	const timer = setTimeout(() => abort.abort(), Math.max(0, limitMs));
	try {
		const { data, response } = await clientForSessionHost(hostId).GET("/api/v1/sessions/{sessionId}/resume-agent", {
			params: { path: { sessionId } },
			signal: abort.signal,
		});
		if (response.status === 404 || response.status === 405) return "gone";
		return data ?? "retry";
	} catch {
		return "retry";
	} finally {
		clearTimeout(timer);
	}
}

/**
 * Before opening resumes a local session, check the resume the daemon already
 * knows: a recorded failure is kept without resuming again, unless the agent
 * was seen running since; a running one is followed instead of being started
 * twice. Returns whether to resume.
 */
export async function shouldResumeOnOpen(sessionId: string, refresh: () => unknown): Promise<boolean> {
	const status = await readResume(sessionId, undefined, RESUME_CHECK_LIMIT_MS);
	if (status === "gone" || status === "retry") return true;
	if (status.state === "running") {
		watchResume(sessionId, undefined, refresh);
		return false;
	}
	if (status.state === "failed" && apiErrorCode(status.error) !== "AGENT_RESUME_IN_PROGRESS" && !seenRunning.has(watchKey(sessionId))) {
		update(watchKey(sessionId), { state: "failed", message: status.error?.message || "Failed to resume agent" });
		return false;
	}
	return true;
}

/** Test seam: forget every watch. */
export function resetResumeWatches(): void {
	watches.clear();
	seenRunning.clear();
	for (const listener of listeners) listener();
}
