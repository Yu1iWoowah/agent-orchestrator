import type { MessageBoxOptions, MessageBoxReturnValue } from "electron";

export class ProtectedSideShutdownRefusal extends Error {
	constructor(public readonly sideId: string, public readonly reason: string, public readonly orphan: boolean) {
		super("A protected side is blocking shutdown.");
	}
}

export function protectedSideShutdownRefusal(status: number, body: unknown): ProtectedSideShutdownRefusal | undefined {
	if (status !== 409 || !body || typeof body !== "object" || !("status" in body) || body.status !== "shutdown_not_settled") return;
	if (!("entry" in body) || body.entry !== "protected_side_chats" || !("protectedSide" in body)) return;
	const side = body.protectedSide;
	if (!side || typeof side !== "object" || !("sideId" in side) || typeof side.sideId !== "string" || !side.sideId) return;
	if (!("reason" in side) || typeof side.reason !== "string" || !side.reason || !("orphan" in side) || typeof side.orphan !== "boolean") return;
	return new ProtectedSideShutdownRefusal(side.sideId, side.reason, side.orphan);
}

export async function showProtectedSideShutdownRefusal(refusal: ProtectedSideShutdownRefusal, options: {
	zh: boolean;
	showMessageBox: (options: MessageBoxOptions) => Promise<Pick<MessageBoxReturnValue, "response">>;
	openEntry(): void;
}): Promise<void> {
	const copy = options.zh ? {
		title: "受保護 side",
		message: "受保護 side 正在擋關機或升版。",
		detail: "請開啟「受保護 side」查看原因，並手動關閉 side 後重試。",
		orphan: "（parent 已結束、刪除或轉為 TUI）",
		open: "開啟受保護 side", stay: "留在 AO",
	} : {
		title: "Protected sides",
		message: "A protected side is blocking shutdown or updates.",
		detail: "Open Protected sides to inspect the reason and explicitly close the side, then retry.",
		orphan: " (parent ended, was deleted, or switched to TUI)",
		open: "Open protected sides", stay: "Stay in AO",
	};
	const answer = await options.showMessageBox({
		type: "warning",
		title: copy.title,
		message: copy.message,
		detail: [copy.detail, refusal.sideId + (refusal.orphan ? copy.orphan : "")].join("\n"),
		buttons: [copy.open, copy.stay],
		defaultId: 1,
		cancelId: 1,
		noLink: true,
	});
	if (answer.response === 0) options.openEntry();
}
