import type { RunFileInfo } from "../shared/daemon-discovery";
import type { DaemonStatus } from "../shared/daemon-status";
import { shutdownRefusalMessage } from "../shared/daemon-takeover";
import { protectedSideShutdownRefusal } from "./protected-side-quit";

export async function requestDesktopShutdown(options: {
	keepAlive: boolean;
	update?: { signal: AbortSignal; confirmDrafts(): boolean };
	appRunId: string;
	status: DaemonStatus;
	preserveAgents(): boolean;
	readRunInfo(): Promise<RunFileInfo | null>;
	requestShutdown(port: number, body: { stopAgents: boolean; appRunId: string }): Promise<{ ok: boolean; status: number; json?(): Promise<unknown> }>;
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
	const response = await options.requestShutdown(info.port, {
		stopAgents: !options.preserveAgents(),
		appRunId: options.appRunId,
	});
	if (!response.ok || response.status !== 202) {
		const body = await response.json?.().catch(() => null);
		throw protectedSideShutdownRefusal(response.status, body) ?? new Error(shutdownRefusalMessage(response.status, body));
	}
	return true;
}
