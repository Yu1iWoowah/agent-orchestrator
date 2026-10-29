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
async function setup(page: Page, scenario: "paged" | "stale" | "empty" | "delete" = "paged") {
	await installFakeAgent(page, {
		projectId,
		workers: [{ id: "worker-current", title: "Current gallery worker", mode: "chat" }],
	});
	const pages: number[] = [],
		reads: unknown[] = [];
	let changed = scenario === "stale",
		deleted = false,
		deletes = 0;
	if (scenario === "delete")
		await page.addInitScript(() =>
			localStorage.setItem(
				"ao.artifacts.view",
				JSON.stringify({ view: "list", pinnedArtifactIds: [JSON.stringify(["worker-current", "worker/file-0.md"])] }),
			),
		);
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
		const all = scenario === "empty" || deleted ? [] : scenario === "delete" ? inventory().slice(0, 1) : inventory(),
			limit = Number(url.searchParams.get("limit"));
		await route.fulfill({
			json: {
				catalog: {
					projectId,
					items: all.slice(offset, offset + limit),
					errors:
						scenario === "empty" || scenario === "delete"
							? [{ source: "shared", code: "DOCUMENT_SOURCE_MISSING" }]
							: [{ source: "worker", sessionId: "missing-worker", code: "DOCUMENT_SOURCE_MISSING" }],
					incomplete: true,
					revision: "r1",
					...(offset + limit < all.length ? { nextCursor: String(offset + limit) } : {}),
				},
			},
		});
	});
	await page.route("**/api/v1/sessions/worker-current/artifact-files?**", async (route) => {
		if (route.request().method() !== "DELETE") {
			await route.continue();
			return;
		}
		expect(new URL(route.request().url()).searchParams.get("path")).toBe("worker/file-0.md");
		deletes++;
		deleted = true;
		await route.fulfill({ status: 204 });
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
		deleteCount: () => deletes,
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
	await expect(page.getByText(/missing-worker: This document is unavailable/)).toBeVisible();
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
	const state = await setup(page, "stale");
	await expect(page.getByText(/Files changed. Refresh to continue/)).toBeVisible();
	await expect(page.getByText("200 files loaded")).toBeVisible();
	state.refresh();
	await page.getByRole("button", { name: "Refresh", exact: true }).click();
	await expect(page.getByText("1107 files loaded")).toBeVisible({ timeout: 15_000 });
	await expect(page.getByText(/Files changed. Refresh to continue/)).toHaveCount(0);
});

test("@P0 an uninitialized Shared source has a known empty state, not a source failure", async ({ page }) => {
	await setup(page, "empty");
	await expect(page.getByText("0 files loaded", { exact: true })).toBeVisible();
	await expect(page.getByRole("heading", { name: "Create your first artifact" })).toBeVisible();
	await expect(page.getByText("Total unavailable", { exact: true })).toHaveCount(0);
	await expect(page.getByText("Some sources could not be loaded")).toHaveCount(0);
	await expect(page.getByText("Not created yet")).toHaveCount(0);
	await page.getByRole("button", { name: "Filter by source" }).click();
	await page.getByRole("menuitemradio", { name: "Shared", exact: true }).click();
	await expect(page.getByText("Not created yet", { exact: true })).toHaveCount(1);
});
test("@P0 Worker deletion confirms, refreshes catalog and removes legacy and v2 pins", async ({ page }) => {
	const state = await setup(page, "delete");
	await expect(page.getByText("1 files loaded", { exact: true })).toBeVisible();
	await expect(page.getByRole("button", { name: "Artifacts", exact: true })).toHaveText("Artifacts1");
	const pinned = page.getByRole("region", { name: "Pinned", exact: true });
	await expect(pinned).toBeVisible();
	await pinned.getByRole("button", { name: /More actions/ }).click();
	await page.getByRole("menuitem", { name: "Delete artifact", exact: true }).click();
	const dialog = page.getByRole("dialog");
	await expect(dialog).toBeVisible();
	expect(state.deleteCount()).toBe(0);
	await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
	expect(state.deleteCount()).toBe(0);
	await pinned.getByRole("button", { name: /More actions/ }).click();
	await page.getByRole("menuitem", { name: "Delete artifact", exact: true }).click();
	await dialog.getByRole("button", { name: "Delete artifact", exact: true }).click();
	await expect(page.getByText("0 files loaded", { exact: true })).toBeVisible();
	await expect(pinned).toHaveCount(0);
	await expect(page.getByText("worker/file-0.md", { exact: true })).toHaveCount(0);
	expect(state.deleteCount()).toBe(1);
	expect(
		await page.evaluate(() => JSON.parse(localStorage.getItem("ao.artifacts.view") ?? "{}").pinnedArtifactIds),
	).toEqual([]);
	await expect(page.getByRole("button", { name: "Artifacts", exact: true })).toHaveText("Artifacts");
});
