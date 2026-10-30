import { expect, test } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

test("chat tabs stay clipped before the header actions in a narrow pane @P0", async ({ page }) => {
	await installFakeAgent(page, { workers: [{ id: "narrow-chat", title: "Long session title beside Awaiting you", mode: "chat" }] });
	await page.route("**/api/v1/sessions/narrow-chat/conversation{,?*}", (route) => route.fulfill({ json: {
		conversationId: "narrow-conversation", sessionId: "narrow-chat", harness: "codex", mode: "chat", controller: "ready",
		latestSequence: 0, oldestSequence: 0, hasMoreBefore: false, turns: [], messages: [], activities: [], settings: {},
	} }));
	await page.goto("/#/projects/fake-proj/sessions/narrow-chat");
	const region = page.getByTestId("session-terminal-region");
	await expect(region).toBeVisible();
	await region.evaluate((element) => { element.style.maxWidth = "60px"; });
	await expect.poll(() => region.evaluate((element) => {
		const bounds = element.getBoundingClientRect();
		// No part of the owning tab (including its raised menu) can hit a point
		// beyond the pane, even when its label is wider than the available strip.
		return document.elementFromPoint(bounds.right + 8, bounds.y + bounds.height / 2)
			?.closest('[data-terminal-role="primary"]') === null;
	})).toBe(true);
	const scroller = region.locator(".session-tab-scroll-region");
	await expect.poll(() => scroller.evaluate((element) => element.scrollWidth - element.clientWidth)).toBeGreaterThan(0);
	await scroller.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
	await expect.poll(() => scroller.evaluate((element) => element.scrollLeft)).toBeGreaterThan(0);
});
