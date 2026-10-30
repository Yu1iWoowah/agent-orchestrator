import path from "node:path";
import { mkdir, writeFile } from "node:fs/promises";
import { _electron as electron, expect, test as base, type Page } from "@playwright/test";
import { agentSettingsOperation, agentSettingsSelection, agentSettingsSnapshot } from "../src/renderer/test/agent-settings-fixtures";
import type { components } from "../src/api/schema";
import { installFakeAgent } from "./support/fake-bridge";
import { openInspector } from "./support/open-inspector";

declare global {
	interface Window {
		__aoSettingsSandboxProof?: { rendererSandbox: boolean; contextIsolated: boolean };
	}
}

// Native Electron uses the same deterministic HTTP/SSE boundaries as renderer
// CI. No daemon, native provider, account credential or paid tier is contacted.
const test = process.env.AO_E2E_ELECTRON ? base.extend({
	page: async ({}, use, info) => {
		const app = await electron.launch({ chromiumSandbox: true, executablePath: require("electron") as string, args: [path.join(__dirname, "support/settings-electron.cjs")], recordVideo: { dir: info.outputPath("video"), size: { width: 1440, height: 960 } } });
		try {
			const page = await app.firstWindow();
			const isolation = await app.evaluate(({ app }) => ({
				electron: process.versions.electron,
				userData: app.getPath("userData"),
				noSandbox: app.commandLine.hasSwitch("no-sandbox"),
			}));
			await page.waitForFunction(() => window.__aoSettingsSandboxProof !== undefined);
			const renderer = await page.evaluate(() => window.__aoSettingsSandboxProof);
			const proofPath = info.outputPath("electron-isolation.json");
			await mkdir(info.outputDir, { recursive: true });
			await writeFile(proofPath, JSON.stringify({ ...isolation, ...renderer }, null, 2));
			await info.attach("electron-isolation", { path: proofPath, contentType: "application/json" });
			expect(isolation.noSandbox).toBe(false);
			expect(renderer?.rendererSandbox).toBe(true);
			expect(renderer?.contextIsolated).toBe(true);
			page.on("pageerror", (error) => console.error("Electron renderer error:", error.message));
			page.on("console", (message) => { if (message.type() === "error" && !message.text().includes("Failed to load resource")) console.error("Electron console:", message.text()); });
			await page.bringToFront(); await use(page);
		}
		finally { await app.close(); }
	},
}) : base;
const sessionId = "sess-1";
const origin = `http://127.0.0.1:${process.env.AO_E2E_PORT ?? 5173}`;
const now = new Date().toISOString();
const longTitle = "Isolated settings validation — retain the unsent message and current turn — ".repeat(4);
const longModel = `isolated-model-${"long-provider-model-name-".repeat(12)}`;

async function installSettings(page: Page, mode: "chat" | "tui", harness = "codex", options: { pauseReads?: boolean; unknownEffective?: boolean; theme?: "dark" | "light" } = {}) {
	let snapshot = agentSettingsSnapshot({ pending: undefined, restartRequired: mode === "tui" });
	snapshot.capabilities.models.push({ model: longModel, isDefault: false, efforts: ["low", "high"], fast: "supported" });
	if (options.unknownEffective) snapshot.effective = { model: { state: "unknown", reason: "observation_unavailable" }, effort: { state: "unknown", reason: "observation_unavailable" }, fastMode: { state: "unknown", reason: "observation_unavailable" } };
	let releaseReads: () => void = () => {};
	const readBarrier = options.pauseReads ? new Promise<void>((resolve) => { releaseReads = resolve; }) : Promise.resolve();
	let busy = true;
	let stopped = false;
	let resumedFast: string | undefined;
	let offline = false;
	let conflict = false;
	let patches = 0;
	const requests: components["schemas"]["ChangeSessionAgentSettingsPayload"][] = [];
	await page.addInitScript((theme) => localStorage.setItem("ao.theme", theme), options.theme ?? "dark");
	await installFakeAgent(page, { daemonPort: 43791, workers: [{ id: sessionId, title: longTitle, provider: harness, mode, activity: "active", createdAt: now }] });
	await page.route("**/api/v1/**", async (route) => {
		const request = route.request(), url = new URL(request.url()), routePath = url.pathname;
		if (routePath.endsWith("/agent-settings")) {
			if (offline) { await route.abort("connectionfailed"); return; }
			if (request.method() === "PATCH") {
				patches++;
				const body = request.postDataJSON() as components["schemas"]["ChangeSessionAgentSettingsPayload"];
				requests.push(body);
				if (conflict || body.expectedRevision !== snapshot.revision) {
					await route.fulfill({ status: 409, json: { error: "conflict", code: "AGENT_SETTINGS_CONFLICT", message: "A managed operation or started operation owns this session; retry after it finishes.", requestId: "electron-conflict" } }); return;
				}
				const target = { model: body.settings.model === undefined ? snapshot.settings.model : body.settings.model ?? "", effort: body.settings.effort === undefined ? snapshot.settings.effort : body.settings.effort ?? "", fastMode: body.settings.fastMode ?? snapshot.settings.fastMode };
				snapshot = { ...snapshot, pending: agentSettingsOperation({ target, applyMode: body.applyMode ?? "after-turn", stopTurn: body.stopTurn }) };
				await route.fulfill({ status: 202, json: { operation: snapshot.pending } }); return;
			}
			await readBarrier;
			await route.fulfill({ json: snapshot }); return;
		}
		if (routePath.endsWith("/conversation")) {
			await route.fulfill({ json: { conversationId: "settings-conversation", sessionId, harness, mode: "chat", controller: stopped ? "stopped" : busy ? "busy" : "ready", settings: { model: snapshot.pending?.target.model ?? snapshot.settings.model, reasoningEffort: snapshot.pending?.target.effort ?? snapshot.settings.effort, fastMode: snapshot.settings.fastMode === "on" }, oldestSequence: 1, latestSequence: 1, hasMoreBefore: false, turns: busy ? [{ id: "turn-1", state: "running", requestedAt: now, startedAt: now }] : [], messages: [{ kind: "message", id: "user-1", turnId: "turn-1", sequence: 1, revision: 0, role: "user", origin: "human", text: "Keep working while I choose the next settings.", streaming: false, createdAt: now }], activities: [] } }); return;
		}
		if (routePath.endsWith("/resume-agent")) { resumedFast = snapshot.settings.fastMode; stopped = false; await route.fulfill({ json: { sessionId, mode: "native" } }); return; }
		if (routePath.endsWith("/conversation/models")) { await route.fulfill({ json: { models: snapshot.capabilities.models.map((model) => ({ id: model.model, displayName: model.model, default: model.isDefault, efforts: model.efforts, supportsFast: true })), selected: { model: snapshot.settings.model } } }); return; }
		if (routePath.endsWith("/conversation/config-options")) { await route.fulfill({ json: { options: [] } }); return; }
		if (routePath.endsWith("/conversation/skills")) { await route.fulfill({ json: { skills: [] } }); return; }
		if (routePath.endsWith("/attention")) { await route.fulfill({ json: { conversationId: "settings-conversation", activeBranchId: "root", counts: { open: 0, unread: 0, history: 0 }, hasMoreBefore: false, items: [] } }); return; }
		if (routePath.endsWith("/questions")) { await route.fulfill({ json: { items: [], open: 0, unread: 0, history: 0, hasMoreBefore: false, nextBeforeSequence: 0 } }); return; }
		if (routePath.endsWith("/plan")) { await route.fulfill({ json: { state: "absent", conversationId: "settings-conversation", activeBranchId: "root" } }); return; }
		if (routePath.endsWith("/workspace/files")) { await route.fulfill({ json: { files: [], truncated: false } }); return; }
		if (routePath.endsWith("/accounts")) { await route.fulfill({ json: { accounts: [] } }); return; }
		await route.fulfill({ status: 404, json: { error: { code: "NOT_FOUND", message: "No isolated fixture for this route" } } });
	});
	const emit = async () => page.evaluate((id) => { window.__aoFakeAgent!.setStatus(id, "idle", "idle"); window.__aoFakeAgent!.metadataChanged(id); }, sessionId);
	return {
		releaseReads,
		requests,
		patches: () => patches,
		stopForResume: () => { stopped = true; busy = false; },
		resumedFast: () => resumedFast,
		apply: async () => {
			if (!snapshot.pending) throw new Error("No pending settings");
			const applied = { ...snapshot.pending, phase: "applied" as const };
			snapshot = { ...snapshot, settings: applied.target, revision: snapshot.revision + 1, pending: undefined, history: [applied], effective: { model: { state: "known", value: applied.target.model }, effort: { state: "known", value: applied.target.effort }, fastMode: { state: "known", value: applied.target.fastMode } } };
			busy = false; await emit();
		},
		external: async () => {
			const selection = agentSettingsSelection({ model: "external-model" });
			const entry = agentSettingsOperation({ id: "external-operation", actor: { kind: "orchestrator", sessionId: "verified-orchestrator" }, phase: "applied", source: snapshot.settings, target: selection, baseRevision: snapshot.revision });
			snapshot = { ...snapshot, revision: snapshot.revision + 1, settings: selection, history: [entry, ...snapshot.history ?? []] }; await emit();
		},
		offline: (value: boolean) => { offline = value; },
		conflict: (value: boolean) => { conflict = value; },
	};
}

async function openSettings(page: Page) {
	await page.goto(`${origin}/#/projects/fake-proj/sessions/${sessionId}`);
	const inspector = await openInspector(page);
	const emptyFrame = page.getByRole("button", { name: "Collapse Awaiting you", exact: true });
	if (await emptyFrame.isVisible()) await emptyFrame.click();
	await inspector.getByRole("button", { name: "Agent settings", exact: true }).click();
	await expect(inspector.getByRole("group", { name: "Current selection" })).toBeVisible();
	await page.evaluate(() => { document.documentElement.dataset.theme = "dark"; });
	return inspector;
}

test("settings preserve the busy turn, drafts, fast resume and conflict evidence in Electron @T0", async ({ page }, info) => {
	await page.setViewportSize({ width: 1440, height: 960 });
	const state = await installSettings(page, "chat", "claude-code");
	await page.goto(`${origin}/#/projects/fake-proj/sessions/${sessionId}`);
	const inspector = await openInspector(page);
	const emptyFrame = page.getByRole("button", { name: "Collapse Awaiting you", exact: true });
	if (await emptyFrame.isVisible()) await emptyFrame.click();
	if (process.env.AO_U4_BEFORE) { await page.screenshot({ path: info.outputPath("before-agent-settings.png"), fullPage: true }); return; }
	await inspector.getByRole("button", { name: "Agent settings", exact: true }).click();
	await expect(inspector.getByRole("group", { name: "Current selection" })).toBeVisible();
	const message = page.getByLabel("Message the agent", { exact: true });
	await message.fill("Unsent draft must survive external settings and resume");
	await inspector.getByRole("button", { name: "Edit agent settings" }).click();
	let dialog = page.getByRole("dialog", { name: "Change agent settings" });
	await dialog.getByRole("button", { name: /^Model:/ }).click();
	await page.getByRole("menuitem", { name: "pending-model", exact: true }).click();
	await dialog.getByRole("button", { name: /^Fast:/ }).click();
	await page.getByRole("menuitem", { name: "on", exact: true }).click();
	await dialog.getByRole("button", { name: "Apply settings" }).click();
	await expect(page.getByTestId("agent-settings-composer")).toContainText("pending-model");
	await expect(inspector.getByRole("group", { name: "Current selection" })).toContainText("committed-model");
	await expect(inspector.getByRole("group", { name: "Provider effective values" })).toContainText("Not provided");
	await inspector.getByText("Fast observation details", { exact: true }).focus();
	await page.keyboard.press("Enter");
	await expect(inspector.getByText("provider_not_provided", { exact: true })).toBeVisible();
	await page.screenshot({ path: info.outputPath("effective-reason-keyboard.png"), fullPage: true });
	await expect(message).toHaveText("Unsent draft must survive external settings and resume");
	await page.screenshot({ path: info.outputPath("busy-pending-dark.png"), fullPage: true });
	await state.apply();
	await expect(inspector.getByRole("group", { name: "Current selection" })).toContainText("pending-model");
	await expect(page.getByTestId("agent-settings-composer")).toHaveCount(0);
	await page.screenshot({ path: info.outputPath("applied-dark.png"), fullPage: true });
	state.stopForResume();
	await page.reload();
	await page.getByRole("button", { name: "Resume agent", exact: true }).click();
	await expect.poll(() => state.resumedFast()).toBe("on");
	await expect(page.getByTestId("agent-settings-header")).toContainText("fast on");
	await expect(message).toHaveText("Unsent draft must survive external settings and resume");
	await page.screenshot({ path: info.outputPath("fast-native-resume.png"), fullPage: true });
	// Reopen after reload if persisted Inspector disclosure differs by client.
	if (!await inspector.isVisible()) await openInspector(page);
	if (!await inspector.getByRole("button", { name: "Edit agent settings" }).isVisible()) await inspector.getByRole("button", { name: "Agent settings", exact: true }).click();
	await inspector.getByRole("button", { name: "Edit agent settings" }).focus();
	await page.keyboard.press("Enter");
	dialog = page.getByRole("dialog", { name: "Change agent settings" });
	await dialog.getByRole("button", { name: /^Model:/ }).click();
	await page.getByRole("menuitem", { name: "committed-model", exact: true }).click();
	await state.external();
	await expect(dialog.getByText(/Settings changed elsewhere/)).toBeVisible();
	await expect(dialog.getByRole("button", { name: /^Model:/ })).toContainText("committed-model");
	await dialog.getByRole("button", { name: "Apply settings" }).click();
	await expect(dialog.getByRole("alert")).toContainText("electron-conflict");
	await page.screenshot({ path: info.outputPath("conflict-draft.png"), fullPage: true });
	await page.keyboard.press("Escape");
	await expect(inspector.getByRole("button", { name: "Edit agent settings" })).toBeFocused();
	await expect(message).toHaveText("Unsent draft must survive external settings and resume");
	await expect(page.getByTestId("agent-settings-activities")).toContainText("By orchestrator verified-orchestrator");
	await page.screenshot({ path: info.outputPath("verified-orchestrator-activity.png"), fullPage: true });
	expect(state.patches()).toBe(2);
});

test("TUI settings require explicit restart and support narrow light keyboard controls @T0", async ({ page }, info) => {
	test.skip(Boolean(process.env.AO_U4_BEFORE), "Before capture uses the same Chat session as the after capture");
	await page.setViewportSize({ width: 960, height: 720 });
	const state = await installSettings(page, "tui", "codex", { theme: "light" });
	const inspector = await openSettings(page);
	await page.evaluate(() => { document.documentElement.classList.remove("dark"); document.documentElement.dataset.theme = "light"; });
	await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
	await inspector.getByRole("button", { name: "Edit agent settings" }).focus();
	await page.keyboard.press("Enter");
	const dialog = page.getByRole("dialog", { name: "Change agent settings" });
	await dialog.getByRole("button", { name: /^Fast:/ }).focus();
	await page.keyboard.press("Enter");
	await page.getByRole("menuitem", { name: "on", exact: true }).focus();
	await page.keyboard.press("Enter");
	await dialog.getByRole("button", { name: "Review restart" }).focus();
	await page.keyboard.press("Enter");
	const confirmation = page.getByRole("dialog", { name: "Restart this session" });
	await expect(confirmation.getByRole("checkbox", { name: "Allow stopping the current turn" })).not.toBeChecked();
	expect(state.patches()).toBe(0);
	await page.screenshot({ path: info.outputPath("tui-restart-light-narrow.png"), fullPage: true });
	await confirmation.getByRole("button", { name: "Confirm restart" }).focus();
	await page.keyboard.press("Enter");
	await expect.poll(() => state.patches()).toBe(1);
	expect(state.requests[0]).toMatchObject({ applyMode: "restart", stopTurn: false });
	await state.apply();
	await expect(inspector.getByRole("group", { name: "Current selection" })).toContainText("on");
	await page.screenshot({ path: info.outputPath("tui-restarted-light.png"), fullPage: true });
});

test("settings show loading and unknown and preserve a long-model draft through offline retry @T0", async ({ page }, info) => {
	test.skip(Boolean(process.env.AO_U4_BEFORE), "Before capture uses the same Chat session as the after capture");
	await page.setViewportSize({ width: 1100, height: 800 });
	const state = await installSettings(page, "chat", "codex", { pauseReads: true, unknownEffective: true });
	await page.goto(`${origin}/#/projects/fake-proj/sessions/${sessionId}`);
	const inspector = await openInspector(page);
	const emptyFrame = page.getByRole("button", { name: "Collapse Awaiting you", exact: true });
	if (await emptyFrame.isVisible()) await emptyFrame.click();
	await inspector.getByRole("button", { name: "Agent settings", exact: true }).click();
	await expect(inspector.getByText("Loading agent settings…")).toBeVisible();
	await page.screenshot({ path: info.outputPath("loading.png"), fullPage: true });
	state.releaseReads();
	await expect(inspector.getByRole("group", { name: "Provider effective values" }).getByText("Unknown", { exact: true })).toHaveCount(3);
	const message = page.getByLabel("Message the agent", { exact: true });
	await message.fill("Keep this draft while disconnected");
	await inspector.getByRole("button", { name: "Edit agent settings" }).click();
	const dialog = page.getByRole("dialog", { name: "Change agent settings" });
	await dialog.getByRole("button", { name: /^Model:/ }).click();
	await page.getByRole("menuitem", { name: longModel, exact: true }).click();
	state.offline(true);
	await dialog.getByRole("button", { name: "Apply settings" }).click();
	await expect(dialog.getByRole("alert")).toBeVisible();
	await expect(dialog.getByRole("button", { name: /^Model:/ })).toContainText(longModel);
	await page.screenshot({ path: info.outputPath("offline-long-model.png"), fullPage: true });
	state.offline(false);
	await dialog.getByRole("button", { name: "Retry", exact: true }).click();
	await expect(dialog).toHaveCount(0);
	await expect(page.getByTestId("agent-settings-composer")).toContainText(longModel);
	await expect(message).toHaveText("Keep this draft while disconnected");
	await state.apply();
	await expect(inspector.getByRole("group", { name: "Current selection" })).toContainText(longModel);
	await page.setViewportSize({ width: 960, height: 720 });
	await page.screenshot({ path: info.outputPath("long-model-narrow.png"), fullPage: true });
	const bodyOverflows = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
	expect(bodyOverflows).toBe(false);
});
