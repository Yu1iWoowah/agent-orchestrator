import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";
const sessionId = "document-reader-chat",
	now = "2026-10-29T12:00:00Z";
const root = "ao://sessions/other-project/worker/artifacts/";
async function setup(page: Page, attentionEnabled = false) {
	let incoming = false;
	const answers: unknown[] = [];
	const seen = new Set<string>();
	const input = (id: string, sequence: number) => ({
		branchId: "root",
		actionable: true,
		read: seen.has(id),
		activity: {
			kind: "activity",
			id,
			turnId: "turn",
			requestId: `native-${id}`,
			activityKind: "user_input",
			status: "pending",
			sequence,
			revision: 0,
			summary:
				id === "original"
					? "Original provider request"
					: "New provider request",
			createdAt: now,
			detail: {
				inputMode: "form",
				message:
					id === "original"
						? "Original provider request"
						: "New provider request",
				schema: {
					type: "object",
					required: ["target"],
					properties: { target: { type: "string", title: "Target" } },
				},
			},
		},
	});
	const items = () =>
		attentionEnabled
			? [input("original", 2), ...(incoming ? [input("new", 3)] : [])]
			: [];
	const attention = () => ({
		conversationId: "document-conversation",
		activeBranchId: "root",
		items: items(),
		counts: {
			open: items().length,
			unread: items().filter((i) => !i.read).length,
			history: 0,
		},
		hasMoreBefore: false,
	});
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
		if (path.endsWith("/attention")) await route.fulfill({ json: attention() });
		else if (path.includes("/attention/") && path.endsWith("/read")) {
			seen.add(path.split("/").at(-2)!);
			await route.fulfill({ status: 204 });
		} else if (path.includes("/attention/"))
			await route.fulfill({
				json: {
					...attention(),
					items: items().filter(
						(i) => i.activity.id === path.split("/").at(-1),
					),
				},
			});
		else if (path.includes("/inputs/") && path.endsWith("/resolve")) {
			answers.push(route.request().postDataJSON());
			await route.fulfill({ status: 204 });
		} else if (path.endsWith("/plan") && attentionEnabled)
			await route.fulfill({
				json: {
					state: "present",
					conversationId: "document-conversation",
					activeBranchId: "root",
					version: {
						branchId: "root",
						turnId: "turn",
						sourceActivityId: "plan",
						sequence: 1,
						revision: 0,
						updatedAt: now,
						plan: {
							steps: [
								{ text: "Plan state stays intact", status: "in_progress" },
							],
						},
					},
				},
			});
		else if (path.endsWith("/conversation"))
			await route.fulfill({
				json: {
					conversationId: "document-conversation",
					activeBranchId: "root",
					sessionId,
					harness: "claude-code",
					mode: "chat",
					controller: "ready",
					settings: {},
					oldestSequence: 1,
					latestSequence: attentionEnabled ? (incoming ? 3 : 2) : 1,
					hasMoreBefore: false,
					turns: [
						{
							id: "turn",
							state: attentionEnabled ? "running" : "completed",
							requestedAt: now,
						},
					],
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
					activities: items().map((i) => i.activity),
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
	return {
		answerCount: () => answers.length,
		addQuestion: async () => {
			incoming = true;
			await page.evaluate(
				(id) =>
					window.__aoFakeAgent!.conversationChanged(
						id,
						"document-conversation",
					),
				sessionId,
			);
		},
	};
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
				.evaluate((el) => (el as HTMLImageElement).naturalWidth),
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

test("document Markdown follows the selected app theme independently of OS preference @T0", async ({
	page,
}) => {
	await setup(page);
	await page.emulateMedia({ colorScheme: "light" });
	await page.evaluate(async (path) => {
		const { useUiStore } = await import(path);
		useUiStore.getState().setThemePreference("dark");
	}, "/src/renderer/stores/ui-store.ts");
	await page.getByRole("link", { name: "Worker guide", exact: true }).click();
	const body = page.locator("[data-document-scroll] .markdown-body");
	await expect(body).toHaveCSS("background-color", "rgba(0, 0, 0, 0)");
	const colors = await body.evaluate((el) => {
		const probe = document.createElement("span");
		probe.style.color = "var(--foreground)";
		el.append(probe);
		const expected = getComputedStyle(probe).color;
		probe.remove();
		return { actual: getComputedStyle(el).color, expected };
	});
	expect(colors.actual).toBe(colors.expected);
});

test("opening the temporary document keeps its first frame inside the Chat safe area @T0", async ({
	page,
}) => {
	await setup(page);
	await page.emulateMedia({ reducedMotion: "no-preference" });
	await page.getByRole("link", { name: "Worker guide", exact: true }).click();
	const sheet = page.getByRole("dialog", { name: "Document", exact: true });
	const frame = sheet.getByRole("region");
	await frame.waitFor({ state: "visible" });
	const bounds = await frame.boundingBox();
	const composer = await page
		.locator(".cursor-chat-composer-dock")
		.boundingBox();
	expect(bounds).not.toBeNull();
	expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(1440);
	expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(composer!.y - 8);
});

test("HTML Preview history returns to the top while Markdown retains scroll @T0", async ({
	page,
}) => {
	await setup(page);
	await page.route("**/api/v1/local-documents/content*", async (route) => {
		if (route.request().postDataJSON().path === "unsafe.html") {
			await route.fulfill({
				contentType: "application/octet-stream",
				body: "<h1>HTML history</h1>" + "<p>History paragraph</p>".repeat(150),
			});
		} else await route.fallback();
	});
	await page.getByRole("link", { name: "Worker guide", exact: true }).click();
	const sheet = page.getByRole("dialog", { name: "Document", exact: true });
	await expect(
		sheet.getByRole("heading", { name: "Guide", exact: true }),
	).toBeVisible();
	await sheet.locator("[data-document-scroll]").evaluate((el) => {
		el.scrollTop = 350;
	});
	await sheet.getByRole("button", { name: /^Collapse / }).click();
	await page.getByRole("link", { name: "Unsafe HTML", exact: true }).click();
	const frame = page.frameLocator("iframe[data-document-static]");
	await expect(
		frame.getByRole("heading", { name: "HTML history" }),
	).toBeVisible();
	await frame.locator("body").evaluate(() => window.scrollTo(0, 600));
	await expect
		.poll(() => frame.locator("body").evaluate(() => window.scrollY))
		.toBe(600);
	await sheet.locator("[data-document-scroll]").evaluate((el) => {
		el.scrollTop = 30;
	});
	await sheet.getByRole("button", { name: "Back", exact: true }).click();
	await expect(
		sheet.getByRole("heading", { name: "Guide", exact: true }),
	).toBeVisible();
	await expect
		.poll(() =>
			sheet.locator("[data-document-scroll]").evaluate((el) => el.scrollTop),
		)
		.toBe(350);
	await sheet.getByRole("button", { name: "Forward", exact: true }).click();
	await expect(
		frame.getByRole("heading", { name: "HTML history" }),
	).toBeVisible();
	await expect
		.poll(() => frame.locator("body").evaluate(() => window.scrollY))
		.toBe(0);
	await expect
		.poll(() =>
			sheet.locator("[data-document-scroll]").evaluate((el) => el.scrollTop),
		)
		.toBe(0);
	await expect(page.locator("iframe[data-document-static]")).toHaveAttribute(
		"sandbox",
		"",
	);
});

test("documents preserve Plan and provider drafts while new attention stays reachable without stealing focus @T0", async ({
	page,
}) => {
	const fixture = await setup(page, true);
	const attention = page.getByRole("region", {
		name: "Awaiting you",
		exact: true,
	});
	await attention
		.getByRole("button", {
			name: "Go to original request: Original provider request",
			exact: true,
		})
		.click();
	const provider = page.getByTestId("elicitation-dock");
	const answer = provider.getByRole("textbox", { name: /^Target/ });
	await answer.fill("Preserve this provider draft");
	await page.getByRole("button", { name: "Open Plan", exact: true }).click();
	await expect(
		page.getByRole("region", { name: "Plan", exact: true }),
	).toContainText("Plan state stays intact");
	const preferences = () =>
		page.evaluate(async (path) => {
			const { useConversationFrameStore } = await import(path);
			return JSON.stringify(useConversationFrameStore.getState().sessions);
		}, "/src/renderer/stores/conversation-frame-store.ts");
	const before = await preferences();
	await page.getByRole("link", { name: "Worker guide", exact: true }).focus();
	await page
		.getByRole("link", { name: "Worker guide", exact: true })
		.press("Enter");
	const sheet = page.getByRole("dialog", { name: "Document", exact: true });
	await expect(
		sheet.getByRole("heading", { name: "Guide", exact: true }),
	).toBeVisible();
	await sheet.getByRole("region").focus();
	await fixture.addQuestion();
	await expect(attention).toContainText("New provider request");
	await expect(sheet.getByRole("region")).toBeFocused();
	await expect(answer).toHaveValue("Preserve this provider draft");
	expect(await preferences()).toBe(before);
	expect(fixture.answerCount()).toBe(0);
	await expect(
		page.getByRole("button", { name: "Open Awaiting you", exact: true }),
	).toHaveAttribute("aria-expanded", "true");
	const hit = await answer.evaluate((el) => {
		const r = el.getBoundingClientRect();
		return el.contains(
			document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2),
		);
	});
	expect(hit).toBe(true);
	await page.getByRole("button", { name: "Search", exact: true }).click();
	const modal = page.getByRole("dialog").last();
	await expect(modal).toBeVisible();
	expect(
		await modal.evaluate((el) => {
			const r = el.getBoundingClientRect();
			return el.contains(
				document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2),
			);
		}),
	).toBe(true);
	await page.keyboard.press("Escape");
	await expect(sheet).toBeVisible();
	await page
		.getByRole("button", { name: "Open Awaiting you", exact: true })
		.click();
	await expect(sheet).toBeHidden();
	await attention
		.getByRole("button", {
			name: "Go to original request: New provider request",
			exact: true,
		})
		.click();
	await expect(answer).toBeFocused();
	await expect(answer).toHaveValue("");
	await attention
		.getByRole("button", {
			name: "Go to original request: Original provider request",
			exact: true,
		})
		.click();
	await expect(answer).toHaveValue("Preserve this provider draft");
	expect(fixture.answerCount()).toBe(0);
	expect(await preferences()).toBe(before);
});
