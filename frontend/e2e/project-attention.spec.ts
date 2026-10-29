import { expect, test } from "@playwright/test";

test("project user decisions stay separate from coordination reports and answer at the original source @T0", async ({
	page,
}) => {
	await page.goto("/e2e/project-attention/harness.html");
	await page.getByRole("button", { name: "Open project attention" }).click();
	await expect(page.getByText("2 items · 2 unread")).toBeVisible();
	await expect(
		page.getByText("Please coordinate this report before involving the user."),
	).toHaveCount(0);
	await page
		.getByRole("button", { name: /Choose a deployment target/ })
		.click();
	const dialog = page.getByRole("dialog");
	await dialog
		.getByRole("radio", { name: "Staging Recommended", exact: true })
		.check();
	await dialog.getByRole("checkbox", { name: "Tests", exact: true }).check();
	await dialog.getByRole("button", { name: "Submit all answers" }).click();
	await expect(page.locator("html")).toHaveAttribute("data-answer-posts", "1");
	await expect(
		dialog.getByText("Saved · Pending delivery", { exact: false }),
	).toBeVisible();
	await page.keyboard.press("Escape");
	await expect(page.getByRole("tab", { name: /Awaiting you 1/ })).toBeVisible();
	await page.getByRole("tab", { name: "Orchestrator reports" }).click();
	await expect(
		page.getByText("Answer state unknown · report delivery: acknowledged"),
	).toBeVisible();
	await page
		.getByRole("button", { name: /Please coordinate this report/ })
		.click();
	await expect(
		page
			.getByRole("dialog")
			.getByText("This report is read-only.", { exact: false }),
	).toBeVisible();
	await expect(page.locator("html")).toHaveAttribute("data-report-reads", "1");
	await expect(
		page.getByRole("button", { name: "Submit all answers" }),
	).toHaveCount(0);
});
test("provider summary navigates to the original session without manufacturing consent @T0", async ({
	page,
}) => {
	await page.goto("/e2e/project-attention/harness.html");
	await page.getByRole("button", { name: "Open project attention" }).click();
	await page.getByRole("button", { name: /Allow workspace command/ }).click();
	await expect(page.getByRole("button", { name: "Allow once" })).toHaveCount(0);
	await page.getByRole("button", { name: "Go to original request" }).click();
	await expect(page.getByText("Original worker session")).toBeVisible();
	await expect(page.locator("html")).not.toHaveAttribute("data-approval-posts");
});
test("refresh preserves the current editor and Browser; offline counts remain unknown @T0", async ({
	page,
}) => {
	await page.goto("/e2e/project-attention/harness.html");
	const draft = page.getByRole("textbox", { name: "Conversation draft" });
	await draft.fill("Keep my unsent work");
	await page.evaluate(() =>
		window.dispatchEvent(
			new CustomEvent("project-attention-fixture", { detail: "revise" }),
		),
	);
	await expect(draft).toHaveValue("Keep my unsent work");
	await expect(draft).toBeFocused();
	await expect(page.getByRole("dialog")).toHaveCount(0);
	await expect(
		page.getByRole("textbox", { name: "Browser address" }),
	).toHaveValue("https://example.invalid/fixture");
	await page.evaluate(() =>
		window.dispatchEvent(
			new CustomEvent("project-attention-fixture", { detail: "offline" }),
		),
	);
	await expect(page.getByLabel("Awaiting you: ?")).toBeVisible();
	await page.getByRole("button", { name: "Open project attention" }).click();
	await expect(
		page.getByText("The project count is unknown.", { exact: false }),
	).toBeVisible();
});
test("zh-TW project dialog works in a narrow light window with keyboard focus restore @T0", async ({
	page,
}) => {
	await page.setViewportSize({ width: 900, height: 700 });
	await page.goto(
		"/e2e/project-attention/harness.html?locale=zh-TW&theme=light",
	);
	const trigger = page.getByRole("button", { name: "開啟專案待回覆事項" });
	await trigger.focus();
	await trigger.press("Enter");
	await expect(
		page.getByRole("dialog", { name: "專案待回覆事項" }),
	).toBeVisible();
	await page.keyboard.press("Escape");
	await expect(trigger).toBeFocused();
});
