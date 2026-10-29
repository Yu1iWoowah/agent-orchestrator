import { expect, test } from "@playwright/test";
for (const scenario of ["cache-props", "props-dom", "zero-height"]) {
	test(`B1 ${scenario}: real Chromium geometry and capture invariants`, async ({ page }) => {
		await page.goto(`/e2e/app-diagnostics-geometry/harness.html?case=${scenario}`);
		await expect(page.getByTestId("ready")).toHaveText("ready");
		const value = await page.evaluate(() => window.__u11!.capture());
		expect(await page.evaluate(() => window.__u11!.stable())).toBe(true);
		expect(JSON.stringify(value)).not.toContain("CANARY_U11_PRIVATE");
		expect(value.cache.queries[0].workspaces[0].sessions[0].id).toBe("next.js-1");
		const component = value.components.entries.find((row) => row.projectId === "next.js");
		if (scenario === "cache-props") expect(component?.workerIds).toEqual([]);
		else expect(component?.workerIds).toEqual(["next.js-1"]);
		const row = value.geometry.nodes.find((node) => node.role === "session_row");
		if (scenario === "props-dom") expect(row?.availability).toBe("not_mounted");
		if (scenario === "zero-height") {
			expect(row?.style?.box.height).toBeGreaterThan(0);
			expect(row?.ancestors.some((style) => style.box.height === 0 && style.overflowY === "hidden")).toBe(true);
			expect(row?.intersectsViewport).toBe(true);
			expect(row?.hit).not.toBe("clear");
		}
	});
}
