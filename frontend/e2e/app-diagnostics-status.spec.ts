import { expect, test } from "@playwright/test";
for (const lang of ["en", "zh-TW"]) {
	test(`adopted daemon status is read-only and fits the viewport (${lang})`, async ({ page }) => {
		await page.setViewportSize({ width: 480, height: 800 });
		await page.goto(`/e2e/app-diagnostics/harness.html?lang=${lang}`);
		await expect(page.getByRole("status")).toContainText(lang === "en" ? "using an existing daemon" : "沿用既有 daemon");
		await expect(page.getByRole("status")).toContainText(lang === "en" ? "Giving consent again will not fix" : "重新同意無法修復");
		expect(await page.getByRole("switch").count()).toBe(0);
		expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
		await page.goto(`/e2e/app-diagnostics/harness.html?lang=${lang}&state=disabled`);
		await expect(page.getByRole("status")).toHaveCount(0);
	});
}
