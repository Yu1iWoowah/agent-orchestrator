import { expect, it, vi } from "vitest";
import { showUpdateFailure } from "./update-failure";

it.each([0, 1])("offers an explicit restart after the update failed (%s)", async (response) => {
	const relaunch = vi.fn();
	const showMessageBox = vi.fn().mockResolvedValue({ response });
	await showUpdateFailure("zh-TW", { showMessageBox, relaunch });
	expect(showMessageBox).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({
		message: "更新失敗，請重新啟動 AO",
		buttons: ["重新啟動", "關閉"],
		defaultId: 1,
		cancelId: 1,
	}));
	expect(relaunch).toHaveBeenCalledTimes(response === 0 ? 1 : 0);
});
