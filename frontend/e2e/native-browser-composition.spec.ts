import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";
import { openInspector } from "./support/open-inspector";

async function pixel(page: Page, x: number, y: number) {
	const png = await page.screenshot({ clip: { x, y, width: 1, height: 1 }, omitBackground: true });
	return page.evaluate(async (bytes) => {
		const bitmap = await createImageBitmap(new Blob([new Uint8Array(bytes)], { type: "image/png" }));
		const canvas = document.createElement("canvas");
		canvas.width = canvas.height = 1;
		const context = canvas.getContext("2d")!;
		context.drawImage(bitmap, 0, 0);
		bitmap.close();
		return [...context.getImageData(0, 0, 1, 1).data];
	}, [...png]);
}

async function themeColor(page: Page, token: string) {
	return page.evaluate((token) => {
		const canvas = document.createElement("canvas");
		canvas.width = canvas.height = 1;
		const context = canvas.getContext("2d")!;
		context.fillStyle = getComputedStyle(document.documentElement).getPropertyValue(token).trim();
		context.fillRect(0, 0, 1, 1);
		return [...context.getImageData(0, 0, 1, 1).data];
	}, token);
}

test("Settings restores its canvas while the live Browser stays mounted @P0", async ({ page }) => {
	await installFakeAgent(page, { workers: [{ id: "native-settings", title: "Browser settings regression", mode: "tui" }] });
	await page.addInitScript(() => {
		localStorage.setItem("ao.theme", "dark");
		localStorage.setItem("ao.theme-style", "dracula");
	});
	await page.goto("/#/projects/fake-proj/sessions/native-settings");
	await page.evaluate(() => { document.documentElement.dataset.nativeBrowserComposition = "true"; });
	await openInspector(page);
	await page.locator("#inspector").getByRole("tab", { name: "Browser", exact: true }).click();
	await page.getByPlaceholder("Search or enter a URL").fill("http://localhost:4173/");
	await page.getByPlaceholder("Search or enter a URL").press("Enter");
	await expect(page.getByTestId("browser-panel")).toHaveAttribute("data-browser-native-page", "live");
	await page.getByRole("button", { name: "Settings", exact: true }).click();
	await expect(page.getByTestId("settings-page")).toBeVisible();
	await expect(page.locator("html")).not.toHaveAttribute("data-browser-hole", "true");
	expect(await pixel(page, 350, 180)).toEqual(await themeColor(page, "--color-surface"));
	await expect(page.getByTestId("browser-panel")).toHaveAttribute("data-browser-native-page", "live");
});

for (const theme of ["dark", "light"]) {
	test(`native browser only clears the visible viewport in ${theme} theme @P0`, async ({ page }) => {
		await page.goto(`/e2e/native-browser-composition/harness.html?theme=${theme}`);
		const header = await themeColor(page, "--color-sidebar");
		const canvas = await themeColor(page, "--color-bg-primary");
		// A mounted but hidden Browser must not clear any shell background.
		await page.locator("#session").evaluate((element) => element.classList.add("hidden"));
		expect(await pixel(page, 400, 10)).toEqual(header);
		expect(await pixel(page, 400, 200)).toEqual(canvas);
		await page.locator("#session").evaluate((element) => element.classList.remove("hidden"));
		await page.evaluate(() => {
			const root = document.documentElement;
			root.dataset.browserHole = "true";
			root.dataset.browserNativeLive = "true";
			for (const [key, value] of Object.entries({ x: 600, y: 120, w: 300, h: 400 })) {
				document.querySelector<HTMLElement>(".app-shell-root")!.style.setProperty(`--browser-hole-${key}`, `${value}px`);
			}
		});
		expect(await pixel(page, 400, 10)).toEqual(header);
		expect((await pixel(page, 700, 300))[3]).toBe(0);
		// Settings retains the hidden session DOM, including its live marker.
		await page.evaluate(() => {
			delete document.documentElement.dataset.browserHole;
			delete document.documentElement.dataset.browserNativeLive;
			document.getElementById("session")!.classList.add("hidden");
			document.getElementById("settings")!.classList.remove("hidden");
		});
		expect(await pixel(page, 400, 10)).toEqual(header);
		expect(await pixel(page, 400, 200)).toEqual(canvas);
	});
}
