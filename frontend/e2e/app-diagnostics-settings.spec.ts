import { expect, test } from "@playwright/test";
for (const lang of ["en", "zh-TW"]) {
	for (const theme of ["light", "dark"]) {
		test(`Settings consent renderer lifecycle ${lang}/${theme}`, async ({ page }) => {
			await page.setViewportSize({ width: 480, height: 800 });
			await page.goto(`/e2e/app-diagnostics-settings/harness.html?lang=${lang}&theme=${theme}&error=cleanup`);
			const allow = page.getByRole("button", { name: lang === "en" ? "Allow for 15 minutes" : "允許 15 分鐘" });
			await expect(allow).toBeEnabled();
			await expect(page.getByRole("combobox")).toBeVisible();
			expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
			await allow.focus();
			await page.keyboard.press("Enter");
			const revoke = page.getByRole("button", { name: lang === "en" ? "Revoke" : "撤銷" });
			await expect(revoke).toBeVisible();
			await page.getByRole("button", { name: lang === "en" ? "View audit" : "查看稽核" }).click();
			await expect(page.getByRole("table")).toContainText("512");
			await revoke.click();
			await expect(page.getByRole("alert")).toContainText(lang === "en" ? "Access is disabled" : "存取已停用");
			await expect(allow).toBeVisible();
			expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
		});
	}
}
