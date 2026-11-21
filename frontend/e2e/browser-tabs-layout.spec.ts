import { expect, test } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";
import { openInspector } from "./support/open-inspector";

for (const theme of ["dark", "light"] as const) {
	for (const mode of ["docked", "compact"] as const) {
		for (const count of [1, 3]) {
			test(`browser ${theme} ${mode} keeps ${count} tabs usable without duplicating the single page title @P0`, async ({ page }, testInfo) => {
				await page.emulateMedia({ reducedMotion: "reduce" });
				await page.setViewportSize({ width: mode === "docked" ? 1800 : 1100, height: 900 });
				await installFakeAgent(page, { workers: [{ id: "browser-layout", title: "Browser tab layout", mode: "tui" }] });
				await page.addInitScript(({ count, theme }) => {
					localStorage.setItem("ao.theme", theme);
					localStorage.setItem("ao.theme-style", theme === "dark" ? "dracula" : "orchestrate");
					let tabs = Array.from({ length: count }, (_, index) => ({
						id: `t${index + 1}`, url: `http://localhost:4173/page-${index + 1}`, title: count === 1 ? "AO guarded navigation with a long page title for browser layout verification" : `Page ${index + 1}`, active: index === 0,
					}));
					let activeTabId = "t1";
					window.ao!.browser.getRuntimeState = async () => ({ connected: true, connecting: false, rejected: false });
					const state = (viewId: string) => ({ viewId, activeTabId, tabs: tabs.map((tab) => ({ ...tab, active: tab.id === activeTabId })) });
					window.ao!.browser.ensure = async (sessionId) => ({
						viewId: `preview:${sessionId}`, url: tabs[0]?.url ?? "", title: tabs[0]?.title ?? "",
						canGoBack: false, canGoForward: false, isLoading: false,
					});
					window.ao!.browser.getTabs = async (viewId) => state(viewId);
					window.ao!.browser.selectTab = async ({ viewId, tabId }) => {
						activeTabId = tabId;
						return state(viewId);
					};
					window.ao!.browser.closeTab = async ({ viewId, tabId }) => {
						tabs = tabs.filter((tab) => tab.id !== tabId);
						if (activeTabId === tabId) activeTabId = tabs[0]?.id ?? "";
						return state(viewId);
					};
				}, { count, theme });
				await page.goto("/#/projects/fake-proj/sessions/browser-layout");
				await openInspector(page);
				await page.locator("#inspector").getByRole("tab", { name: "Browser", exact: true }).click();
				const address = page.getByTestId("browser-address-bar");
				await expect(address).toBeVisible();
				await expect(address.getByRole("textbox")).toHaveValue("localhost");
				await page.evaluate(() => document.fonts.ready);
				await testInfo.attach("layout", { body: await page.screenshot({ animations: "disabled" }), contentType: "image/png" });
				const geometry = await page.evaluate(() => {
					const rect = (selector: string) => document.querySelector(selector)?.getBoundingClientRect().toJSON();
					const strip = document.querySelector<HTMLElement>(".browser-panel__tab-strip");
					return {
						panel: rect('[data-testid="browser-panel"]'), address: rect('[data-testid="browser-address-bar"]'),
						strip: rect(".browser-panel__tab-strip"), scrollWidth: strip?.scrollWidth, clientWidth: strip?.clientWidth,
					};
				});
				await testInfo.attach("geometry", { body: JSON.stringify({ theme, mode, count, ...geometry }), contentType: "application/json" });
				expect(Math.abs(geometry.address!.width - (mode === "docked" ? 240 : 180))).toBeLessThanOrEqual(0.1);
				await expect(page.locator(".browser-panel__topbar-host").getByRole("tab")).toHaveCount(0);
				const strip = page.getByRole("tablist", { name: "Browser tabs", exact: true });
				if (count === 1) {
					await expect(strip.getByRole("tab")).toHaveCount(1);
					await expect(strip.getByRole("tab")).toContainText("AO guarded navigation with a long page title for browser layout verification");
					const close = page.getByRole("button", { name: "Close tab AO guarded navigation with a long page title for browser layout verification", exact: true });
					await expect(close).toBeVisible();
					await close.click();
					await expect(strip.getByRole("tab")).toHaveCount(0);
				} else {
					await expect(page.getByTestId("browser-tab-row").getByRole("tablist", { name: "Browser tabs" })).toHaveCount(1);
					await expect(strip.getByRole("tab")).toHaveCount(3);
					expect(geometry.strip!.width).toBeGreaterThan(mode === "docked" ? 450 : 150);
					if (mode === "docked") expect(geometry.scrollWidth! - geometry.clientWidth!).toBeLessThanOrEqual(1);
					await strip.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
					const close = page.getByRole("button", { name: "Close tab Page 3", exact: true });
					const box = await close.boundingBox();
					const view = await strip.boundingBox();
					expect(box!.x).toBeGreaterThanOrEqual(view!.x);
					expect(box!.x + box!.width).toBeLessThanOrEqual(view!.x + view!.width + 1);
					await close.click();
					await expect(strip.getByRole("tab")).toHaveCount(2);
					await strip.getByRole("tab", { name: "Page 1", exact: true }).focus();
					// A page can close another tab while focus remains in this strip.
					await page.getByRole("button", { name: "Close tab Page 2", exact: true }).evaluate((button: HTMLButtonElement) => button.click());
					await expect(strip.getByRole("tab", { name: "Page 1", exact: true })).toBeFocused();
					await expect(strip.getByRole("tab")).toHaveCount(1);
				}
				await expect(page.getByRole("button", { name: "Open new tab", exact: true })).toBeVisible();
			});
		}
	}
}
