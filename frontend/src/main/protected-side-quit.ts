import type { ChildProcess } from "node:child_process";
import type { MessageBoxOptions, MessageBoxReturnValue } from "electron";

/** POST /shutdown keeps admission fenced after a successful preflight. */
export async function requestUpdateShutdown(baseUrl: string): Promise<void> {
	const response = await fetch(`${baseUrl}/shutdown`, { method: "POST", signal: AbortSignal.timeout(15_000) });
	if (response.status !== 202) throw new Error(`AO could not prepare shutdown for the update (HTTP ${response.status}). Inspect Protected sides in Settings, then retry.`);
}

/** A successful shutdown is irreversible; a failed install must start a new daemon. */
export function createUpdateShutdownLifecycle(options: {
	confirmLeave(): boolean;
	requestShutdown(): Promise<void>;
	restoreDaemon(): Promise<void>;
	setApproved(approved: boolean): void;
}) {
	let shutdownAccepted = false;
	let recovery: Promise<void> | undefined;
	return {
		async prepare(): Promise<boolean> {
			await recovery;
			if (!options.confirmLeave()) { options.setApproved(false); return false; }
			try { await options.requestShutdown(); }
			catch (error) { options.setApproved(false); throw error; }
			shutdownAccepted = true;
			options.setApproved(true);
			return true;
		},
		recover(): Promise<void> {
			options.setApproved(false);
			if (recovery) return recovery;
			if (!shutdownAccepted) return Promise.resolve();
			shutdownAccepted = false;
			recovery = options.restoreDaemon().finally(() => { recovery = undefined; });
			return recovery;
		},
	};
}

/** Wait for the exact shutdown target, not just its closed HTTP listener. */
export async function waitForUpdateDaemonExit(options: {
	child: Pick<ChildProcess, "exitCode" | "signalCode"> | null;
	pid: number | undefined;
	isProcessAlive(pid: number): boolean;
}): Promise<void> {
	if (!options.child && !options.pid) throw new Error("The update daemon has no process identity for recovery.");
	const deadline = Date.now() + 15_000;
	while (options.child
		? options.child.exitCode === null && options.child.signalCode === null
		: Boolean(options.pid && options.isProcessAlive(options.pid))) {
		if (Date.now() >= deadline) throw new Error("The update daemon is still stopping; recovery could not start a replacement.");
		await new Promise<void>((resolve) => setTimeout(resolve, 100));
	}
}

/** Only the protected-side veto is handled here; other shutdown reasons keep their existing flow. */
export async function protectedSideBlockers(baseUrl: string): Promise<string[]> {
	const response = await fetch(`${baseUrl}/api/v1/side-chats/protected`, { signal: AbortSignal.timeout(5_000) });
	if (!response.ok) throw new Error(`Protected sides: HTTP ${response.status}`);
	const body: unknown = await response.json();
	if (!body || typeof body !== "object" || !("sides" in body) || !Array.isArray(body.sides)) throw new Error("Invalid protected sides response");
	return body.sides.flatMap((side: unknown) => {
		if (!side || typeof side !== "object" || !("sideId" in side) || typeof side.sideId !== "string" || !("protection" in side)) throw new Error("Invalid protected side");
		const protection = side.protection;
		if (!protection || typeof protection !== "object" || !("shutdownBlocked" in protection) || typeof protection.shutdownBlocked !== "boolean") throw new Error("Invalid protected side status");
		return protection.shutdownBlocked ? [side.sideId] : [];
	});
}

export function protectedSideQuitCopy(zh: boolean) {
	return zh ? {
		title: "受保護 side",
		message: "受保護 side 正在擋關機或升版。",
		detail: "請從 Sidebar 或設定的「受保護 side」入口查看原因，並手動關閉 side 後重試。",
		open: "開啟受保護 side", stay: "留在 AO",
	} : {
		title: "Protected sides",
		message: "Protected sides are blocking shutdown or updates.",
		detail: "Open Protected sides in the sidebar or Settings to inspect the reason and explicitly close the side, then retry.",
		open: "Open protected sides", stay: "Stay in AO",
	};
}

export async function confirmProtectedSideQuit(baseUrl: string, options: {
	zh: boolean;
	showMessageBox: (options: MessageBoxOptions) => Promise<Pick<MessageBoxReturnValue, "response">>;
	openEntry: () => void;
}): Promise<boolean> {
	const copy = protectedSideQuitCopy(options.zh);
	try {
		const blocked = await protectedSideBlockers(baseUrl);
		if (blocked.length === 0) return true;
		const answer = await options.showMessageBox({ type: "warning", title: copy.title, message: copy.message,
			detail: `${copy.detail}\n${blocked.join(", ")}`, buttons: [copy.open, copy.stay], defaultId: 1, cancelId: 1 });
		if (answer.response === 0) options.openEntry();
	} catch (error) {
		await options.showMessageBox({ type: "error", title: copy.title, message: error instanceof Error ? error.message : String(error), buttons: [copy.stay] });
	}
	return false;
}
