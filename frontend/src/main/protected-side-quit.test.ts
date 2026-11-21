import { expect, it, vi } from "vitest";
import { ProtectedSideShutdownRefusal, protectedSideShutdownRefusal, showProtectedSideShutdownRefusal } from "./protected-side-quit";

const body = {
	status: "shutdown_not_settled",
	reason: "protected_side",
	entry: "protected_side_chats",
	protectedSide: { sideId: "old-side", reason: "scope_host_alive", orphan: true },
};

it("reads side identity and orphan status from the structured shutdown refusal", () => {
	expect(protectedSideShutdownRefusal(409, body)).toMatchObject({
		sideId: "old-side", reason: "scope_host_alive", orphan: true,
	});
	expect(protectedSideShutdownRefusal(200, body)).toBeUndefined();
	expect(protectedSideShutdownRefusal(409, { ...body, protectedSide: {} })).toBeUndefined();
	expect(protectedSideShutdownRefusal(409, { status: "shutdown_not_settled", blockedReason: "delivery_unverified" })).toBeUndefined();
});

it.each([0, 1])("opens the settings entry only when the operator chooses it (%s)", async (response) => {
	const openEntry = vi.fn();
	const showMessageBox = vi.fn().mockResolvedValue({ response });
	await showProtectedSideShutdownRefusal(new ProtectedSideShutdownRefusal("old-side", "scope_host_alive", true), { zh: true, showMessageBox, openEntry });
	expect(openEntry).toHaveBeenCalledTimes(response === 0 ? 1 : 0);
	expect(showMessageBox).toHaveBeenCalledWith(expect.objectContaining({
		detail: expect.stringContaining("old-side（parent 已結束、刪除或轉為 TUI）"),
		buttons: ["開啟受保護 side", "留在 AO"],
		defaultId: 1,
	}));
});
