import { afterEach, expect, it, vi } from "vitest";
import { confirmProtectedSideQuit, protectedSideBlockers, protectedSideQuitCopy } from "./protected-side-quit";
afterEach(() => vi.unstubAllGlobals());
it("names every blocking side across launches and leaves released sides alone", async () => {
	vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ sides: [
		{ sideId: "orphan", protection: { shutdownBlocked: true } },
		{ sideId: "old-launch", protection: { shutdownBlocked: true } },
		{ sideId: "released", protection: { shutdownBlocked: false } },
	] }))));
	expect(await protectedSideBlockers("http://127.0.0.1:1234")).toEqual(["orphan", "old-launch"]);
});
it.each([true, false])("points the blocked quit/update prompt to the global entry (zh=%s)", (zh) => {
	const copy = protectedSideQuitCopy(zh);
	expect(copy.detail).toContain(zh ? "受保護 side" : "Protected sides");
	expect(copy.open).toBe(zh ? "開啟受保護 side" : "Open protected sides");
});
it("does not mistake a failed status read for permission to quit", async () => {
	vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("{}", { status: 500 })));
	await expect(protectedSideBlockers("http://127.0.0.1:1234")).rejects.toThrow("HTTP 500");
});

it.each([0, 1])("keeps quit/update blocked and opens the global entry only when chosen (%s)", async (response) => {
	vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ sides: [{ sideId: "orphan", protection: { shutdownBlocked: true } }] }))));
	const openEntry = vi.fn();
	const showMessageBox = vi.fn().mockResolvedValue({ response });
	expect(await confirmProtectedSideQuit("http://127.0.0.1:1234", { zh: true, showMessageBox, openEntry })).toBe(false);
	expect(openEntry).toHaveBeenCalledTimes(response === 0 ? 1 : 0);
	expect(showMessageBox).toHaveBeenCalledWith(expect.objectContaining({ detail: expect.stringContaining("orphan"), buttons: ["開啟受保護 side", "留在 AO"] }));
});
it("permits quit/update after protected sides have been explicitly closed", async () => {
	vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ sides: [] }))));
	const showMessageBox = vi.fn();
	expect(await confirmProtectedSideQuit("http://127.0.0.1:1234", { zh: false, showMessageBox, openEntry: vi.fn() })).toBe(true);
	expect(showMessageBox).not.toHaveBeenCalled();
});
