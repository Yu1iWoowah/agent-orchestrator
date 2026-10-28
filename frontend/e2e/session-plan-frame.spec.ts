import { expect, test } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

test("session Plan floats without moving Chat, restores preferences and hard-fits above the composer @T0", async ({
	page,
}) => {
	const sessionId = "session-plan-frame";
	const now = "2026-10-28T12:00:00Z";
	await page.setViewportSize({ width: 1440, height: 900 });
	await installFakeAgent(page, {
		workers: [{ id: sessionId, title: "A long session with a persistent Plan", mode: "chat" }],
	});
	await page.route(`**/api/v1/sessions/${sessionId}/**`, async (route) => {
		const path = new URL(route.request().url()).pathname;
		if (path.endsWith("/plan"))
			await route.fulfill({
				json: {
					state: "present",
					conversationId: "plan-conversation",
					activeBranchId: "root",
					version: {
						branchId: "root",
						turnId: "old-turn",
						sourceActivityId: "old-plan",
						sequence: 2,
						revision: 3,
						updatedAt: now,
						plan: {
							spansTurns: true,
							steps: [{ text: "Plan from outside the loaded page", status: "in_progress" }],
						},
					},
				},
			});
		else if (path.endsWith("/conversation"))
			await route.fulfill({
				json: {
					conversationId: "plan-conversation",
					sessionId,
					harness: "claude-code",
					mode: "chat",
					controller: "ready",
					settings: {},
					oldestSequence: 250,
					latestSequence: 251,
					hasMoreBefore: false,
					turns: [{ id: "recent", state: "completed", requestedAt: now }],
					messages: [
						{
							kind: "message",
							id: "recent-message",
							turnId: "recent",
							sequence: 250,
							revision: 0,
							role: "user",
							origin: "human",
							text: "A recent message without the old Plan",
							streaming: false,
							createdAt: now,
						},
					],
					activities: [],
				},
			});
		else if (path.endsWith("/conversation/models"))
			await route.fulfill({ json: { models: [], selected: {} } });
		else if (path.endsWith("/conversation/skills")) await route.fulfill({ json: { skills: [] } });
		else if (path.endsWith("/workspace/files"))
			await route.fulfill({ json: { files: [], truncated: false } });
		else
			await route.fulfill({
				status: 404,
				json: { error: { code: "NOT_FOUND", message: "not found" } },
			});
	});
	await page.goto(`/#/projects/fake-proj/sessions/${sessionId}`);
	const entry = page.getByRole("button", { name: "Plan 0/1", expanded: false });
	await expect(entry).toBeVisible();
	const composer = page.locator(".cursor-chat-composer-dock");
	const before = await composer.boundingBox();
	await entry.click();
	const frame = page.getByRole("region", { name: "Plan", exact: true });
	await expect(frame.getByText("Plan from outside the loaded page")).toBeVisible();
	expect(await composer.boundingBox()).toEqual(before);
	const transcript = page.getByRole("log", { name: "Conversation" });
	await expect(transcript.getByText("Plan from outside the loaded page")).toHaveCount(0);
	const fits = async () => {
		const f = await frame.boundingBox();
		const c = await composer.boundingBox();
		expect(f).not.toBeNull();
		expect(c).not.toBeNull();
		if (!f || !c) throw new Error("Missing frame or composer geometry");
		expect(f.y + f.height).toBeLessThanOrEqual(c.y - 15);
	};
	await fits();
	const width = await frame.evaluate((node) => node.getBoundingClientRect().width);
	await frame.getByRole("separator", { name: "Resize Plan width" }).press("ArrowLeft");
	await expect
		.poll(() => frame.evaluate((node) => node.getBoundingClientRect().width))
		.toBe(width + 16);
	await page.reload();
	await expect(frame).toBeVisible();
	await expect
		.poll(() => frame.evaluate((node) => node.getBoundingClientRect().width))
		.toBe(width + 16);
	await fits();
	await page.setViewportSize({ width: 900, height: 350 });
	await expect(frame).not.toBeVisible();
	await page.getByRole("button", { name: "Open Plan", exact: true }).click();
	const compact = page.getByRole("dialog", { name: "Plan", exact: true });
	await expect(compact).toBeVisible();
	const compactRect = await compact.boundingBox();
	const dockRect = await composer.boundingBox();
	if (!compactRect || !dockRect) throw new Error("Missing compact geometry");
	expect(compactRect.y + compactRect.height).toBeLessThanOrEqual(dockRect.y - 15);
	await compact.getByRole("button", { name: "Collapse Plan" }).click();
	await page.setViewportSize({ width: 1440, height: 900 });
	await expect(frame).toBeVisible();
	await frame.getByRole("button", { name: "Collapse Plan" }).click();
	await page.reload();
	await expect(entry).toBeVisible();
});

test("independent frame slots resize, preserve drafts and clear an expanding composer @T0", async ({
	page,
}) => {
	await page.setViewportSize({ width: 1280, height: 900 });
	await page.goto("/e2e/conversation-frames/harness.html?session=dual-frame-e2e");
	const attention = page.getByRole("region", { name: "Awaiting you", exact: true });
	await expect(attention).toBeVisible();
	await page.getByRole("button", { name: "Plan 1/4", expanded: false }).click();
	const plan = page.getByRole("region", { name: "Plan", exact: true });
	await expect(plan).toBeVisible();
	const left = await plan.getByRole("separator", { name: "Resize Plan width" }).boundingBox();
	if (!left) throw Error("Missing resize handle");
	await page.mouse.move(left.x + left.width / 2, left.y + left.height / 2);
	await page.mouse.down();
	await page.mouse.move(left.x - 40, left.y + left.height / 2, { steps: 5 });
	await page.mouse.up();
	await expect
		.poll(() => plan.evaluate((n) => n.getBoundingClientRect().width))
		.toBeGreaterThan(390);
	await page.getByRole("button", { name: "Composer queue", exact: true }).click();
	await expect
		.poll(async () => {
			const dock = await page.locator(".cursor-chat-composer-dock").boundingBox();
			const box = await plan.boundingBox();
			return dock && box ? dock.y - (box.y + box.height) : -1;
		})
		.toBeGreaterThanOrEqual(15);
	const dock = await page.locator(".cursor-chat-composer-dock").boundingBox();
	const box = await plan.boundingBox();
	if (!dock || !box) throw Error("Missing safe geometry");
	expect(box.y + box.height).toBeLessThanOrEqual(dock.y - 15);
	const first = await attention.boundingBox();
	if (!first) throw Error("Missing upper frame");
	expect(first.y + first.height + 11).toBeLessThanOrEqual(box.y);
	await page
		.getByRole("textbox", { name: "Frame draft", exact: true })
		.fill("Keep the independent draft");
	await page.setViewportSize({ width: 800, height: 500 });
	await expect(attention).not.toBeVisible();
	await page.getByRole("button", { name: "Open Awaiting you", exact: true }).click();
	const sheet = page.getByRole("dialog", { name: "Awaiting you", exact: true });
	await expect(sheet).toBeVisible();
	await sheet
		.getByRole("textbox", { name: "Frame draft", exact: true })
		.fill("Keep the compact draft");
	await page.setViewportSize({ width: 1280, height: 900 });
	await expect(sheet.getByRole("textbox", { name: "Frame draft", exact: true })).toHaveValue(
		"Keep the compact draft",
	);
	await sheet.getByRole("button", { name: "Collapse Awaiting you", exact: true }).click();
	await expect(attention).toBeVisible();
});

test("frames and compact Sheets stay inside a viewport narrower than the Chat surface @T0", async ({
	page,
}) => {
	await page.setViewportSize({ width: 640, height: 900 });
	await page.goto("/e2e/conversation-frames/harness.html?session=overflow-frame-e2e");
	await page.locator("[data-conversation-frames]").evaluate((layer) => {
		if (layer.parentElement) layer.parentElement.style.minWidth = "1200px";
	});
	const attention = page.getByRole("region", { name: "Awaiting you", exact: true });
	await expect(attention).toBeInViewport({ ratio: 1 });
	await page.setViewportSize({ width: 640, height: 350 });
	const entry = page.getByRole("button", { name: "Awaiting you", exact: true, expanded: false });
	await expect(entry).toBeInViewport({ ratio: 1 });
	await entry.click();
	const sheet = page.getByRole("dialog", { name: "Awaiting you", exact: true });
	await expect(sheet).toBeInViewport({ ratio: 1 });
	await expect
		.poll(async () => {
			const box = await sheet.boundingBox();
			return box ? box.x + box.width : Infinity;
		})
		.toBeLessThanOrEqual(624);
	const box = await sheet.boundingBox();
	const dock = await page.locator(".cursor-chat-composer-dock").boundingBox();
	if (!box || !dock) throw Error("Missing compact bounds");
	expect(box.y + box.height).toBeLessThanOrEqual(dock.y - 15);
});
