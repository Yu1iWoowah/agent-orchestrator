import type { MessageBoxOptions, MessageBoxReturnValue } from "electron";

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
