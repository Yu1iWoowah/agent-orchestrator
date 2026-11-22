import type { RunFileInfo } from "../shared/daemon-discovery";
import type { DaemonStatus } from "../shared/daemon-status";
import { shutdownRefusalMessage } from "../shared/daemon-takeover";
import { protectedSideShutdownRefusal } from "./protected-side-quit";

export class DesktopShutdownTimeout extends Error {
	constructor() {
		super("The daemon did not confirm shutdown before the deadline.");
	}
}

export async function requestDesktopShutdown(options: {
	keepAlive: boolean;
	update?: { signal: AbortSignal; confirmDrafts(): boolean };
	appRunId: string;
	status: DaemonStatus;
	preserveAgents(): boolean;
	readRunInfo(): Promise<RunFileInfo | null>;
	requestShutdown(port: number, body: { stopAgents: boolean; appRunId: string }, signal: AbortSignal): Promise<{ ok: boolean; status: number; json?(): Promise<unknown> }>;
}): Promise<boolean> {
	if (options.update) {
		options.update.signal.throwIfAborted();
		if (!options.update.confirmDrafts()) throw new Error("Update restart canceled.");
	}
	if (options.keepAlive || options.status.state !== "ready" || !options.status.port) return false;
	const info = await options.readRunInfo();
	if (!info || info.port !== options.status.port) {
		throw new Error("Daemon ownership changed before desktop quit");
	}
	if (info.owner !== "app") return false;
	if (!info.appRunId || info.appRunId !== options.appRunId) {
		throw new Error("Daemon app ownership changed before desktop quit");
	}
	options.update?.signal.throwIfAborted();
	// Allow the daemon's 10-second shutdown preflight to finish and send its response.
	const deadline = AbortSignal.timeout(12_000);
	try {
		const response = await options.requestShutdown(info.port, {
			stopAgents: !options.preserveAgents(),
			appRunId: options.appRunId,
		}, deadline);
		if (response.ok && response.status === 202) return true;
		const body = await response.json?.().catch(() => {
			deadline.throwIfAborted();
			return null;
		});
		throw protectedSideShutdownRefusal(response.status, body) ?? new Error(shutdownRefusalMessage(response.status, body));
	} catch (error) {
		if (deadline.aborted) throw new DesktopShutdownTimeout();
		throw error;
	}
}
