import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";
const sessionId = "document-reader-chat",
	now = "2026-10-29T12:00:00Z";
const root = "ao://sessions/other-project/worker/artifacts/";
async function setup(page: Page) {
	await page.setViewportSize({ width: 1440, height: 1000 });
	await installFakeAgent(page, {
		workers: [
			{
				id: sessionId,
				title: "Current orchestrator conversation",
				mode: "chat",
			},
		],
	});
	await page.route(`**/api/v1/sessions/${sessionId}/**`, async (route) => {
		const path = new URL(route.request().url()).pathname;
		if (path.endsWith("/conversation"))
			await route.fulfill({
				json: {
					conversationId: "document-conversation",
					sessionId,
					harness: "claude-code",
					mode: "chat",
					controller: "ready",
					settings: {},
					oldestSequence: 1,
					latestSequence: 1,
					hasMoreBefore: false,
					turns: [{ id: "turn", state: "completed", requestedAt: now }],
					messages: [
						{
							kind: "message",
							id: "message",
							turnId: "turn",
							sequence: 1,
							revision: 0,
							role: "assistant",
							origin: "agent",
							text: `[Relative HTML](${root}static.html) · [Code](${root}code.ts) · [Worker guide](${root}guide.md) · [Unsafe HTML](${root}unsafe.html) · [Shared](ao://projects/next.js/shared/guide.md) · [Missing](ao://projects/new/shared/guide.md)`,
							streaming: false,
							createdAt: now,
						},
					],
					activities: [],
				},
			});
		else if (path.endsWith("/conversation/models"))
			await route.fulfill({ json: { models: [], selected: {} } });
		else if (path.endsWith("/conversation/skills"))
			await route.fulfill({ json: { skills: [] } });
		else if (path.endsWith("/workspace/files"))
			await route.fulfill({ json: { files: [], truncated: false } });
		else await route.fulfill({ status: 404, json: { code: "NOT_FOUND" } });
	});
	await page.route("**/api/v1/local-documents/**", async (route) => {
		const input = route.request().postDataJSON();
		if (input.projectId === "new") {
			await route.fulfill({
				status: 404,
				json: { code: "DOCUMENT_SOURCE_MISSING", message: "not created" },
			});
			return;
		}
		if (route.request().url().includes("/resolve")) {
			await route.fulfill({
				json: {
					document: {
						target: input,
						localPath: "/managed/" + input.path,
						canonicalUri:
							input.kind === "shared"
								? `ao://projects/${input.projectId}/shared/${input.path}`
								: root + input.path,
						size: 100,
						mediaType: input.path.endsWith("html")
							? "text/html"
							: "text/markdown",
						fingerprint: "v1",
					},
				},
			});
			return;
		}
		if (input.path.endsWith("image.png")) {
			await route.fulfill({
				contentType: "application/octet-stream",
				body: Buffer.from(
					"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jRZkAAAAASUVORK5CYII=",
					"base64",
				),
			});
			return;
		}
		if (input.path.endsWith("style.css")) {
			await route.fulfill({
				contentType: "application/octet-stream",
				body: "h1{color:rgb(20,90,120)}",
			});
			return;
		}
		if (input.path.endsWith("static.html")) {
			await route.fulfill({
				contentType: "application/octet-stream",
				body: '<link rel="stylesheet" href="style.css"><h1>Local resources</h1><img src="image.png" alt="Managed raster"><img src="data:image/svg+xml,<svg onload=top.__documentPwn=99/>" alt="Untrusted data">',
			});
			return;
		}
		const body = input.path.endsWith(".ts")
			? "const answer = 42;\nconsole.log(answer);"
			: input.path.endsWith("html")
				? `<script>top.__documentPwn=1;fetch('/api/v1/pwn')</script><img src="https://document-sink.invalid/image" onerror="top.__documentPwn=2"><form action="http://127.0.0.1:19999/pwn"><button>submit</button></form><meta http-equiv="refresh" content="0;url=https://document-sink.invalid/refresh"><svg onload="top.__documentPwn=3"></svg><style>@import 'https://document-sink.invalid/style';p{background:url(http://127.0.0.1:19999/css)}</style><a href="javascript:top.__documentPwn=4">bad</a><p>Static safe content</p>`
				: "# Guide\n\n[Second heading](#second)\n\n" +
					"Paragraph\n\n".repeat(120) +
					"## Second\n\nEnd";
		await route.fulfill({ contentType: "application/octet-stream", body });
	});
	await page.goto(`/#/projects/fake-proj/sessions/${sessionId}`);
}
test("documents stay beside the current Chat and warm-cache fragments restore inside the viewer @T0", async ({
	page,
}) => {
	await setup(page);
	const before = page.url();
	const composer = page.locator(".cursor-chat-composer-dock");
	const box = await composer.boundingBox();
	await page.getByRole("link", { name: "Worker guide", exact: true }).click();
	const sheet = page.getByRole("region", { name: "guide.md", exact: true });
	await expect(
		sheet.getByRole("heading", { name: "Guide", exact: true }),
	).toBeVisible();
	expect(page.url()).toBe(before);
	expect(await composer.boundingBox()).toEqual(box);
	await sheet.getByRole("link", { name: "Second heading" }).click();
	await expect
		.poll(() =>
			sheet
				.locator("[data-document-scroll]")
				.evaluate((element) => element.scrollTop),
		)
		.toBeGreaterThan(100);
	await page.keyboard.press("Escape");
	await expect(sheet).toBeHidden();
	await page.getByRole("link", { name: "Shared", exact: true }).click();
	await expect(
		page.getByRole("region", { name: "guide.md", exact: true }),
	).toBeVisible();
	expect(page.url()).toBe(before);
	await page.keyboard.press("Escape");
	await page.getByRole("link", { name: "Missing", exact: true }).click();
	await expect(
		page.getByRole("status").filter({ hasText: "Not created yet" }),
	).toBeVisible();
});
test("static HTML has no executable side effects or network/API/top-navigation sinks @T0", async ({
	page,
}) => {
	const sinks: string[] = [];
	page.on("request", (request) => {
		if (/document-sink|:19999|\/api\/v1\/pwn/.test(request.url()))
			sinks.push(request.url());
	});
	await setup(page);
	const before = page.url();
	await page.getByRole("link", { name: "Unsafe HTML", exact: true }).click();
	const frame = page.frameLocator("iframe[data-document-static]");
	await expect(frame.getByText("Static safe content")).toBeVisible();
	await expect(
		frame.locator("script,form,button,meta[http-equiv=refresh],svg,iframe"),
	).toHaveCount(0);
	await frame.getByText("bad", { exact: true }).click();
	await page.waitForTimeout(250);
	expect(
		await page.evaluate(() => Reflect.get(window, "__documentPwn")),
	).toBeUndefined();
	expect(sinks).toEqual([]);
	expect(page.url()).toBe(before);
});

test("read-only code keeps line numbers, copy contents, and original-byte download @T0", async ({
	page,
}) => {
	await setup(page);
	await page.getByRole("link", { name: "Code", exact: true }).click();
	const frame = page.getByRole("region", { name: "code.ts", exact: true });
	await expect(
		frame.locator("diffs-container").locator("[data-line]").first(),
	).toBeVisible();
	await expect(frame.locator('[contenteditable="true"]')).toHaveCount(0);
	await expect(
		frame.getByRole("button", { name: "Copy contents", exact: true }),
	).toBeVisible();
	const download = page.waitForEvent("download");
	await frame.getByRole("button", { name: "Download", exact: true }).click();
	expect((await download).suggestedFilename()).toBe("code.ts");
});

test("document hover previews do not intercept the next link @T0", async ({
	page,
}) => {
	await setup(page);
	await page.getByRole("link", { name: "Worker guide", exact: true }).hover();
	const tooltip = page
		.getByRole("tooltip")
		.filter({ hasText: "/managed/guide.md" });
	await expect(tooltip).toBeVisible();
	await expect(
		tooltip.locator("xpath=ancestor::*[@data-radix-popper-content-wrapper]"),
	).toHaveCSS("pointer-events", "none");
	await page.getByRole("link", { name: "Missing", exact: true }).click();
	await expect(
		page.getByRole("status").filter({ hasText: "Not created yet" }),
	).toBeVisible();
});

test("opaque static HTML loads only verified local raster and styles without enabling script or same-origin @T0", async ({
	page,
}) => {
	await setup(page);
	await page.getByRole("link", { name: "Relative HTML", exact: true }).click();
	const frame = page.frameLocator("iframe[data-document-static]");
	await expect
		.poll(() =>
			frame
				.getByRole("img", { name: "Managed raster" })
				.evaluate((el) => el.naturalWidth),
		)
		.toBe(1);
	await expect(
		frame.getByRole("heading", { name: "Local resources" }),
	).toHaveCSS("color", "rgb(20, 90, 120)");
	await expect(
		frame.getByRole("img", { name: "Untrusted data" }),
	).not.toHaveAttribute("src");
	expect(
		await page.evaluate(() => Reflect.get(window, "__documentPwn")),
	).toBeUndefined();
	await expect(page.locator("iframe[data-document-static]")).toHaveAttribute(
		"sandbox",
		"",
	);
});
