import { expect, test, type Page } from "@playwright/test";

async function openQuestion(page: Page, suffix = "") {
	await page.goto(`/e2e/session-decision/harness.html${suffix}`);
	const row = page.getByRole("button", { name: "Open decision: Choose a deployment target", exact: true });
	await row.press("Enter");
	const dialog = page.getByRole("dialog");
	await expect(dialog.getByText("The build is ready.", { exact: false })).toBeVisible();
	return { dialog, row };
}
async function fillGrouped(page: Page) {
	const dialog = page.getByRole("dialog");
	await dialog.getByRole("radio", { name: "Staging Recommended", exact: true }).check();
	await dialog.getByRole("checkbox", { name: "Tests", exact: true }).check();
	await dialog.getByRole("checkbox", { name: "Other", exact: true }).check();
	await dialog.getByRole("textbox", { name: "Other answer: Checks", exact: true }).fill("Verify rollback\nKeep both answers together");
}

test("grouped decision validates, preserves drafts, and submits one saved receipt @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 1000 });
	const { dialog, row } = await openQuestion(page);
	await expect(dialog.getByRole("radio", { checked: true })).toHaveCount(0);
	await dialog.getByRole("button", { name: "Submit all answers" }).click();
	await expect(dialog.locator("fieldset[aria-invalid=true]")).toHaveCount(2);
	await expect(dialog.locator("fieldset").first()).toBeFocused();
	await fillGrouped(page);
	const textarea = dialog.getByRole("textbox", { name: "Other answer: Checks" });
	await textarea.press("Enter");
	await expect(page.locator("html")).not.toHaveAttribute("data-answer-posts");
	await dialog.locator("summary").filter({ hasText: "Preview: Staging" }).click();
	await expect(dialog.getByText("deploy --target staging")).toBeVisible();
	await expect(dialog.locator("img,iframe,a[href],video,object")).toHaveCount(0);
	await page.keyboard.press("Escape");
	await expect(dialog).toHaveCount(0);
	await expect(row).toBeFocused();
	await row.press("Space");
	await expect(page.getByRole("dialog").getByRole("checkbox", { name: "Tests", exact: true })).toBeChecked();
	await page.getByRole("dialog").getByRole("button", { name: "Submit all answers" }).click();
	await expect(page.getByRole("dialog").getByText("Saved · Pending delivery", { exact: true })).toBeVisible();
	await expect(page.locator("html")).toHaveAttribute("data-answer-posts", "1");
	await expect(page.getByRole("dialog").getByText("Saved · Delivered", { exact: true })).toHaveCount(0);
	await page.evaluate(() => window.dispatchEvent(new CustomEvent("decision-fixture", { detail: "delivered" })));
	await expect(page.getByRole("dialog").getByText("Saved · Delivered", { exact: true })).toBeVisible();
});

test("409 keeps the old draft as a reference and requires new choices @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 1000 });
	await openQuestion(page, "?answer=conflict");
	await fillGrouped(page);
	await page.getByRole("dialog").getByRole("button", { name: "Submit all answers" }).click();
	await expect(page.getByRole("dialog").getByText("The deployment scope changed.", { exact: false })).toBeVisible();
	await expect(page.getByRole("dialog").getByText("Previous draft · version 1 · reference only", { exact: true })).toBeVisible();
	await expect(page.getByRole("dialog").getByRole("radio", { checked: true })).toHaveCount(0);
	await expect(page.getByRole("dialog").getByRole("checkbox", { checked: true })).toHaveCount(0);
});

for (const mode of ["lost", "delay"]) test(`closing during ${mode} response recovers receipt without a second POST @T0`, async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 1000 });
	await openQuestion(page, `?answer=${mode}`);
	await fillGrouped(page);
	const phases = await page.getByRole("dialog").getByRole("button", { name: "Submit all answers" }).evaluate((button) => {
		if (!(button instanceof HTMLButtonElement)) throw new Error("Missing submit button");
		button.click();
		return Object.keys(localStorage).filter((key) => key.startsWith("ao.question.drafts.v2:"))
			.map((key) => JSON.parse(localStorage.getItem(key) ?? "null").phase.state);
	});
	expect(phases).toEqual(["submitting"]);
	await page.keyboard.press("Escape");
	await expect(page.getByRole("dialog")).toHaveCount(0);
	await expect.poll(() => page.evaluate(() => Object.keys(localStorage).filter((key) => key.startsWith("ao.question.drafts.v2:")).length)).toBe(0);
	await page.getByRole("region", { name: "Awaiting you", exact: true }).getByRole("tab", { name: "History", exact: true }).click();
	await page.getByRole("button", { name: "Open decision: Choose a deployment target", exact: true }).click();
	await expect(page.getByRole("dialog").getByText("Saved · Pending delivery", { exact: true })).toBeVisible();
	await expect(page.locator("html")).toHaveAttribute("data-answer-posts", "1");
});

test("another page reaching draft capacity preserves the active form and refuses new persistence @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 1000 });
	const { dialog } = await openQuestion(page);
	await fillGrouped(page);
	await dialog.getByRole("textbox", { name: "Other answer: Checks" }).fill("User is still typing in window A");
	const activeKey = await page.evaluate(() => {
		window.addEventListener("storage", (event) => {
			if (event.key === "decision-capacity-complete") document.documentElement.dataset.capacityComplete = event.newValue ?? "";
		});
		return Object.keys(localStorage).find((key) => key.startsWith("ao.question.drafts.v2:"));
	});
	if (!activeKey) throw new Error("Active draft was not persisted");
	const other = await page.context().newPage();
	try {
		await openQuestion(other);
		const result = await other.evaluate(async ({ active, storePath, fixturePath }) => {
			const { useQuestionDraftStore }: typeof import("../src/renderer/stores/question-draft-store") = await import(storePath);
			const { sessionQuestionFixture }: typeof import("../src/renderer/test/fixtures/session-question") = await import(fixturePath);
			for (let i = 0; i < 32; i++) {
				const key = useQuestionDraftStore.getState().ensure(sessionQuestionFixture({ id: `capacity-${i}` }));
				useQuestionDraftStore.getState().setField(key, "target", { selectedOptionIds: ["prod"], otherSelected: false, otherText: "" });
			}
			const snapshot = { activePresent: localStorage.getItem(active) !== null, available: useQuestionDraftStore.getState().storageAvailable };
			localStorage.setItem("decision-capacity-complete", "done");
			return snapshot;
		}, { active: activeKey, storePath: "/src/renderer/stores/question-draft-store.ts", fixturePath: "/src/renderer/test/fixtures/session-question.ts" });
		expect(result).toEqual({ activePresent: true, available: false });
		await expect(page.locator("html")).toHaveAttribute("data-capacity-complete", "done");
		await expect(dialog.getByRole("radio", { name: /Staging/ })).toBeChecked();
		await expect(dialog.getByRole("textbox", { name: "Other answer: Checks" })).toHaveValue("User is still typing in window A");
		await expect(other.getByRole("dialog").getByText("Your draft is kept in this window, but could not be saved for a restart.")).toBeVisible();
	} finally { await other.close(); }
});

for (const theme of ["light", "dark"]) test(`zh-TW ${theme} effective 200% dialog fits above composer and traps focus @T0`, async ({ page }) => {
	await page.setViewportSize({ width: 720, height: 550 });
	await page.goto(`/e2e/session-decision/harness.html?theme=${theme}&locale=zh-TW`);
	await page.getByRole("button", { name: /^開啟決策/ }).click();
	const dialog = page.getByRole("dialog");
	await expect(dialog.getByRole("button", { name: "一起送出答案", exact: true })).toBeVisible();
	await expect.poll(async () => {
		const box = await dialog.boundingBox(), composer = await page.locator(".cursor-chat-composer-dock").boundingBox();
		return box && composer ? composer.y - box.y - box.height : -1;
	}).toBeGreaterThanOrEqual(15);
	for (let i = 0; i < 12; i++) {
		await page.keyboard.press(i % 2 ? "Shift+Tab" : "Tab");
		expect(await dialog.evaluate((node) => node.contains(document.activeElement))).toBe(true);
	}
	await expect(page.locator("body")).toHaveJSProperty("scrollWidth", await page.evaluate(() => document.body.clientWidth));
});

test("compact Sheet yields one focus trap at minimum usable R and keeps an entry below the floor @T0", async ({ page }) => {
 await page.setViewportSize({ width: 310, height: 380 });
 await page.goto("/e2e/session-decision/harness.html");
 await page.getByRole("button", { name: "Awaiting you 1", expanded: false }).click();
 await page.getByRole("button", { name: "Open decision: Choose a deployment target", exact: true }).click();
 const dialog = page.getByRole("dialog");
 await expect(dialog).toHaveCount(1);
 await expect(dialog.getByRole("button", { name: "Submit all answers" })).toBeVisible();
 const box = await dialog.boundingBox(), composer = await page.locator(".cursor-chat-composer-dock").boundingBox();
 expect(box!.width).toBeGreaterThanOrEqual(260);
 expect(box!.x + box!.width).toBeLessThanOrEqual(310);
 expect(box!.y + box!.height).toBeLessThanOrEqual(composer!.y - 15);
 await page.setViewportSize({ width: 250, height: 320 });
 await expect(page.getByRole("dialog")).toHaveCount(0);
 await expect(page.getByRole("button", { name: /Awaiting you 1/ })).toBeVisible();
});

test("server field errors scroll into view and receive focus without losing choices @T0", async ({ page }) => {
 await page.setViewportSize({ width: 1100, height: 720 });
 await openQuestion(page, "?answer=rejected");
 await fillGrouped(page);
 const dialog = page.getByRole("dialog");
 await dialog.getByRole("button", { name: "Submit all answers" }).click();
 const error = dialog.getByText("The answer was not accepted. Check the question errors and try again.", { exact: true });
 await expect(error).toBeInViewport();
 await expect(dialog.getByText("Review the target before submitting.")).toBeVisible();
 expect(await error.evaluate((node) => document.activeElement?.contains(node))).toBe(true);
 await expect(dialog.getByRole("radio", { name: /Staging/ })).toBeChecked();
 await expect(page.locator("html")).toHaveAttribute("data-answer-posts", "1");
});
