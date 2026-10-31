import { expect, test } from "@playwright/test";
import { installFakeBridge } from "./support/fake-bridge";

const fixtureBaseUrl = process.env.AO_BACKGROUND_FIXTURE_URL;

test("harness background replies stream and survive refresh exactly once @connected", async ({ page, request }) => {
	test.skip(!fixtureBaseUrl, "Set AO_BACKGROUND_FIXTURE_URL to TestHarnessBackgroundBrowserFixture.");
	const metadataResponse = await request.get(`${fixtureBaseUrl}/test/background`);
	expect(metadataResponse.ok()).toBe(true);
	const metadata: { baseUrl: string; sessionId: string } = await metadataResponse.json();
	await installFakeBridge(page, { daemonPort: Number(new URL(metadata.baseUrl).port) });
	await page.goto(`/e2e/harness-background/connected.html?${new URLSearchParams({ fixtureBaseUrl: fixtureBaseUrl! })}`);
	await expect(page.getByTestId("background-events")).toHaveText("Events: connected");
	await expect(page.getByText("AO-PROMPT-COMPLETED", { exact: true })).toBeVisible();
	await page.getByRole("button", { name: "Start background task", exact: true }).click();
	await expect(page.getByText("BACKGROUND-1", { exact: true })).toBeVisible();
	await page.getByRole("button", { name: "Finish background task", exact: true }).click();
	const reply = page.getByText("BACKGROUND-1-COMPLETED", { exact: true });
	await expect(reply).toBeVisible();
	await expect(reply).toHaveCount(1);
	await page.reload();
	await expect(reply).toBeVisible();
	await expect(reply).toHaveCount(1);
	const text = await page.locator("body").innerText();
	expect(text.indexOf("AO-PROMPT-COMPLETED")).toBeLessThan(text.indexOf("BACKGROUND-1-COMPLETED"));
	expect(text).not.toContain("<task-notification>");
	const history = await request.get(`${metadata.baseUrl}/api/v1/sessions/${metadata.sessionId}/history`);
	expect(history.ok()).toBe(true);
	expect(await history.json()).toMatchObject({ total: 2, turns: [
		{ userRequest: "[Harness-triggered turn]", agentResponse: "BACKGROUND-1-COMPLETED" },
		{ agentResponse: "AO-PROMPT-COMPLETED" },
	] });
});
