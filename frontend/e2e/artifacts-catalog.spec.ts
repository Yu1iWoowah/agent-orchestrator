import { expect, test, type Page } from "@playwright/test";
import type { components } from "../src/api/schema";
import { installFakeAgent } from "./support/fake-bridge";
type Item = components["schemas"]["DocumentCatalogItem"];
const projectId = "gallery-project";
function inventory(): Item[] {
	const workers: Item[] = Array.from({ length: 1105 }, (_, index) => ({
		name: `file-${index}.md`,
		path: `worker/file-${index}.md`,
		source: "worker",
		archived: false,
		size: 20,
		fingerprint: "v1",
		updatedAt: "2026-10-29T00:00:00Z",
		target: { kind: "artifact", projectId, sessionId: "worker-current", path: `worker/file-${index}.md` },
	}));
	return [
		...workers,
		{
			name: "guide.md",
			path: "archive/guide.md",
			source: "orchestrator",
			archived: true,
			size: 20,
			fingerprint: "v1",
			updatedAt: "2026-10-28T00:00:00Z",
			target: { kind: "artifact", projectId, sessionId: "orchestrator-replaced", path: "archive/guide.md" },
		},
		{
			name: "guide.md",
			path: "shared/guide.md",
			source: "shared",
			archived: false,
			size: 20,
			fingerprint: "v1",
			target: { kind: "shared", projectId, path: "shared/guide.md" },
		},
	];
}
async function setup(page: Page, stale = false) {
	await installFakeAgent(page, {
		projectId,
		workers: [{ id: "worker-current", title: "Current gallery worker", mode: "chat" }],
	});
	const pages: number[] = [],
		reads: unknown[] = [];
	let changed = stale;
	await page.route("**/api/v1/projects/*/document-catalog?**", async (route) => {
		const url = new URL(route.request().url());
		expect(url.pathname).toBe(`/api/v1/projects/${projectId}/document-catalog`);
		expect(url.searchParams.get("source")).toBe("all");
		expect(url.searchParams.get("archive")).toBe("all");
		const offset = Number(url.searchParams.get("cursor") ?? "0");
		pages.push(offset);
		if (changed && offset) {
			await route.fulfill({ status: 409, json: { code: "DOCUMENT_CATALOG_CHANGED", message: "Files changed" } });
			return;
		}
		const all = inventory(),
			limit = Number(url.searchParams.get("limit"));
		await route.fulfill({
			json: {
				catalog: {
					projectId,
					items: all.slice(offset, offset + limit),
					errors: [{ source: "worker", sessionId: "missing-worker", code: "DOCUMENT_SOURCE_MISSING" }],
					incomplete: true,
					revision: "r1",
					...(offset + limit < all.length ? { nextCursor: String(offset + limit) } : {}),
				},
			},
		});
	});
	await page.route("**/api/v1/local-documents/**", async (route) => {
		const target = route.request().postDataJSON();
		if (route.request().url().includes("/resolve"))
			await route.fulfill({
				json: {
					document: {
						target,
						canonicalUri: `ao://projects/${projectId}/shared/${target.path}`,
						localPath: `/managed/${target.path}`,
						size: 20,
						fingerprint: "v1",
						mediaType: "text/markdown",
					},
				},
			});
		else {
			reads.push(target);
			await route.fulfill({
				contentType: "text/markdown",
				body: "# Shared gallery reader\n\nRead safely beside the gallery.",
			});
		}
	});
	await page.goto("/#/artifacts");
	return {
		pages,
		reads,
		refresh: () => {
			changed = false;
		},
	};
}
test("@P0 gallery paginates all sources, searches past the old cap and opens the same Sheet", async ({ page }) => {
	const state = await setup(page);
	await expect(page.getByText("1107 files loaded")).toBeVisible({ timeout: 15_000 });
	expect(state.pages).toEqual([0, 200, 400, 600, 800, 1000]);
	expect(state.reads).toHaveLength(0);
	await expect(page.locator("main iframe, main img")).toHaveCount(0);
	await expect(page.getByText(/missing-worker: Not created yet/)).toBeVisible();
	await page.getByRole("searchbox", { name: "Search artifacts" }).fill("file-1104");
	await expect(page.getByText("worker/file-1104.md", { exact: true })).toBeVisible();
	await page.getByRole("searchbox").fill("");
	await page.getByRole("button", { name: "Filter by source" }).click();
	await page.getByRole("menuitemradio", { name: "Shared", exact: true }).click();
	await expect(page.getByText("shared/guide.md", { exact: true })).toBeVisible();
	await expect(page.getByRole("menu")).toHaveCount(0);
	await page.getByText("shared/guide.md", { exact: true }).click();
	await expect(page.getByRole("heading", { name: "Shared gallery reader" })).toBeVisible();
	expect(page.url()).toContain("#/artifacts");
	expect(state.reads).toHaveLength(1);
	await expect(page.getByRole("button", { name: "Go to source" })).toHaveCount(0);
});
test("gallery retains filters and scroll across navigation and reload and separates Archive", async ({ page }) => {
	await setup(page);
	await expect(page.getByText("1107 files loaded")).toBeVisible({ timeout: 15_000 });
	const viewport = page.locator("main .chat-scroll-viewport");
	await viewport.hover();
	await page.mouse.wheel(0, 550);
	await expect.poll(() => viewport.evaluate((node) => node.scrollTop)).toBeGreaterThan(300);
	const top = await viewport.evaluate((node) => node.scrollTop);
	await page.reload();
	await expect(page.getByText("1107 files loaded")).toBeVisible({ timeout: 15_000 });
	await expect.poll(() => viewport.evaluate((node) => node.scrollTop)).toBeCloseTo(top, 0);
	await page.getByRole("button", { name: "Filter by Archive" }).click();
	await page.getByRole("menuitemradio", { name: "Archive", exact: true }).click();
	await expect(page.getByText("archive/guide.md", { exact: true })).toBeVisible();
	await expect(page.getByText("shared/guide.md", { exact: true })).toHaveCount(0);
	await page.getByRole("searchbox").fill("archive/");
	await page.reload();
	await expect(page.getByRole("searchbox")).toHaveValue("archive/");
	await expect(page.getByText("archive/guide.md", { exact: true })).toBeVisible();
});
test("gallery keeps healthy metadata on cursor invalidation and offers Refresh", async ({ page }) => {
	const state = await setup(page, true);
	await expect(page.getByText(/Files changed. Refresh to continue/)).toBeVisible();
	await expect(page.getByText("200 files loaded")).toBeVisible();
	state.refresh();
	await page.getByRole("button", { name: "Refresh", exact: true }).click();
	await expect(page.getByText("1107 files loaded")).toBeVisible({ timeout: 15_000 });
	await expect(page.getByText(/Files changed. Refresh to continue/)).toHaveCount(0);
});
