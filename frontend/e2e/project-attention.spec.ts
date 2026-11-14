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
	await expect(page.getByRole("dialog", { name: "Decision", exact: true })).toHaveCount(0);
	await expect(page.getByRole("tab", { name: /Awaiting you 1/ })).toBeVisible();
	await expect(page.getByRole("button", { name: /Choose a deployment target/ })).toHaveCount(0);
	await expect(page.getByRole("button", { name: /Allow workspace command/ })).toBeFocused();
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
	await page.keyboard.press("Escape");
	await expect(
		page.locator('[data-slot="dialog-overlay"][data-state="open"]'),
	).toHaveCount(1);
	await page.getByRole("tab", { name: /Awaiting you/ }).click();
	await page.getByRole("button", { name: /Allow workspace command/ }).click();
	await expect(
		page.getByRole("button", { name: "Go to original request" }),
	).toBeVisible();
	await page.keyboard.press("Escape");
	await page.getByRole("tab", { name: "Orchestrator reports" }).click();
	await expect(
		page.getByText("Answer state unknown · report delivery: acknowledged"),
	).toBeVisible();
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

test("the current orchestrator opens the project question and saves once at its worker source @T0", async ({ page }) => {
	await page.goto("/e2e/project-attention/harness.html?hosting=main&theme=light");
	const frame = page.getByRole("region", { name: "Awaiting you", exact: true });
	await expect(frame.getByText("2", { exact: true })).toBeVisible();
	await frame.getByRole("button", { name: /Choose a deployment target/ }).click();
	const dialog = page.getByRole("dialog", { name: "Decision", exact: true });
	await expect(dialog.getByText(/report-worker/)).toBeVisible();
	await dialog.getByRole("radio", { name: "Staging Recommended", exact: true }).check();
	await dialog.getByRole("checkbox", { name: "Tests", exact: true }).check();
	await dialog.getByRole("button", { name: "Submit all answers" }).click();
	await expect(page.locator("html")).toHaveAttribute("data-answer-posts", "1");
	await expect(dialog).toHaveCount(0);
	await expect(frame.getByText("1", { exact: true })).toBeVisible();
	await expect(page.locator("html")).toHaveAttribute("data-answer-source", "/api/v1/sessions/worker/questions/ask-1/answer");
	await expect(page.locator("html")).toHaveAttribute("data-answer-delivery-state", "queued");
	await expect(frame.getByRole("button", { name: /Choose a deployment target/ })).toHaveCount(0);
});

test("unknown and both host transitions preserve the same draft and permit only one answer @T0", async ({ page }) => {
	await page.goto("/e2e/project-attention/harness.html?hosting=unknown&theme=light");
	await page.getByRole("button", { name: "Open project attention" }).click();
	await page.getByRole("button", { name: /Choose a deployment target/ }).click();
	const dialog = page.getByRole("dialog", { name: "Decision", exact: true });
	const stage = dialog.getByRole("radio", { name: "Staging Recommended", exact: true });
	await expect(stage).toBeDisabled();
	const host = (value: string) => page.evaluate((detail) => window.dispatchEvent(new CustomEvent("question-host-fixture", { detail })), value);
	await host("none");
	await stage.check();
	await dialog.getByRole("checkbox", { name: "Tests", exact: true }).check();
	await host("main");
	await expect(page.getByText("Current orchestrator · main")).toBeVisible();
	await expect(dialog).toHaveCount(1);
	await expect(stage).toBeChecked();
	await host("unknown");
	await expect(stage).toBeDisabled();
	await host("none");
	await expect(page.getByText("Project attention fixture")).toBeVisible();
	await expect(stage).toBeChecked();
	await expect(stage).toBeEnabled();
	await dialog.getByRole("button", { name: "Submit all answers" }).click();
	await expect(page.locator("html")).toHaveAttribute("data-answer-posts", "1");
	await expect(dialog).toHaveCount(0);
});
