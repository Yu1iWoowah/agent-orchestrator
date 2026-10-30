import path from "node:path";
import { mkdir, writeFile } from "node:fs/promises";
import { createServer, type ServerResponse } from "node:http";
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

async function installSettings(page: Page, mode: "chat" | "tui", harness = "codex", options: { pauseReads?: boolean; unknownEffective?: boolean; theme?: "dark" | "light"; idle?: boolean; immediateApplied?: boolean; realCdcPort?: number } = {}) {
	let snapshot = agentSettingsSnapshot({ pending: undefined, restartRequired: mode === "tui" });
	snapshot.capabilities.models.push({ model: longModel, isDefault: false, efforts: ["low", "high"], fast: "supported" });
	if (options.unknownEffective) snapshot.effective = { model: { state: "unknown", reason: "observation_unavailable" }, effort: { state: "unknown", reason: "observation_unavailable" }, fastMode: { state: "unknown", reason: "observation_unavailable" } };
	let releaseReads: () => void = () => {};
	const readBarrier = options.pauseReads ? new Promise<void>((resolve) => { releaseReads = resolve; }) : Promise.resolve();
	let busy = !options.idle;
	let conversationReads = 0;
	let settingsReads = 0;
	let stopped = false;
	let resumedFast: string | undefined;
	let offline = false;
	let conflict = false;
	let patches = 0;
	let legacyBarrier: Promise<void> = Promise.resolve();
	let releaseLegacy = () => {};
	let legacyRefused = false;
	const legacyRequests: components["schemas"]["ConversationTurnSettingsPayload"][] = [];
	const requests: components["schemas"]["ChangeSessionAgentSettingsPayload"][] = [];
	await page.addInitScript((theme) => localStorage.setItem("ao.theme", theme), options.theme ?? "dark");
	await installFakeAgent(page, { daemonPort: options.realCdcPort ?? 43791, nativeEvents: Boolean(options.realCdcPort), workers: [{ id: sessionId, title: longTitle, provider: harness, mode, activity: options.idle ? "idle" : "active", createdAt: now }] });
	await page.route("**/api/v1/**", async (route) => {
		const request = route.request(), url = new URL(request.url()), routePath = url.pathname;
		if (options.realCdcPort && routePath === "/api/v1/events" && url.port === String(options.realCdcPort)) { await route.continue(); return; }
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
				const operation = agentSettingsOperation({ id: `settings-op-${patches}`, target, applyMode: body.applyMode ?? "after-turn", stopTurn: body.stopTurn, phase: options.immediateApplied ? "applied" : "pending" });
				snapshot = options.immediateApplied
					? { ...snapshot, settings: target, revision: snapshot.revision + 1, pending: undefined, history: [operation], effective: { model: { state: "known", value: target.model }, effort: { state: "known", value: target.effort }, fastMode: { state: "known", value: target.fastMode } } }
					: { ...snapshot, pending: operation };
				await route.fulfill({ status: options.immediateApplied ? 200 : 202, json: { operation } }); return;
			}
			await readBarrier;
			await route.fulfill({ json: snapshot }); settingsReads++; return;
		}
		if (routePath.endsWith("/conversation")) {
			conversationReads++;
			await route.fulfill({ json: { conversationId: "settings-conversation", sessionId, harness, mode: "chat", controller: stopped ? "stopped" : busy ? "busy" : "ready", settings: { model: options.idle ? snapshot.settings.model : snapshot.pending?.target.model ?? snapshot.settings.model, reasoningEffort: options.idle ? snapshot.settings.effort : snapshot.pending?.target.effort ?? snapshot.settings.effort, fastMode: snapshot.settings.fastMode === "on" }, oldestSequence: 1, latestSequence: 1, hasMoreBefore: false, turns: busy ? [{ id: "turn-1", state: "running", requestedAt: now, startedAt: now }] : [], messages: [{ kind: "message", id: "user-1", turnId: "turn-1", sequence: 1, revision: 0, role: "user", origin: "human", text: "Keep working while I choose the next settings.", streaming: false, createdAt: now }], activities: [] } }); return;
		}
		if (routePath.endsWith("/conversation/settings") && request.method() === "PATCH") {
			const body: components["schemas"]["ConversationTurnSettingsPayload"] = request.postDataJSON();
			legacyRequests.push(body);
			await legacyBarrier;
			if (legacyRefused) {
				await route.fulfill({ status: 409, json: { code: "AGENT_SETTINGS_CONFLICT", message: "Wait for the managed operation", requestId: "legacy-choice-request" } }); return;
			}
			const target = agentSettingsSelection({ model: body.model ?? "", effort: body.reasoningEffort ?? "", fastMode: body.fastMode === undefined ? snapshot.settings.fastMode : body.fastMode ? "on" : "off" });
			snapshot = { ...snapshot, pending: agentSettingsOperation({ id: `legacy-op-${legacyRequests.length}`, target }) };
			await route.fulfill({ json: { ...body, model: target.model, reasoningEffort: target.effort, fastMode: target.fastMode === "on" } });
			await emit(); return;
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
		recover: () => {
			busy = false;
			snapshot = { ...snapshot, pending: agentSettingsOperation({ phase: "recovery_required", checkpoint: "applying_model", applyMode: "restart", startedAt: now }) };
		},
		requests,
		conversationReads: () => conversationReads,
		settingsReads: () => settingsReads,
		legacyRequests,
		holdChoice: () => { legacyBarrier = new Promise<void>((resolve) => { releaseLegacy = resolve; }); },
		releaseChoice: (refuse: boolean) => { legacyRefused = refuse; releaseLegacy(); legacyBarrier = Promise.resolve(); },
		patches: () => patches,
		fail: async () => {
			if (!snapshot.pending) throw new Error("No pending settings");
			const failed = { ...snapshot.pending, phase: "failed" as const, checkpoint: "restored" as const, errorCategory: "provider" as const, errorCode: "AGENT_SETTINGS_FAILED", requestId: `failed-request-${patches}` };
			snapshot = { ...snapshot, pending: undefined, history: [failed, ...snapshot.history ?? []] };
			busy = false; await emit();
		},
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
	await expect(page.getByTestId("agent-settings-composer").getByRole("status")).toHaveCount(0);
	await expect(page.getByTestId("agent-settings-composer").getByRole("alert")).toHaveCount(0);
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

test("recovery archive explains cleanup and can be cancelled without archiving @T0", async ({ page }, info) => {
	test.skip(Boolean(process.env.AO_U4_BEFORE), "The original main has no settings recovery control");
	await page.setViewportSize({ width: 960, height: 720 });
	const state = await installSettings(page, "tui");
	state.recover();
	let archives = 0;
	page.on("request", (request) => { if (request.method() === "POST" && new URL(request.url()).pathname.endsWith("/kill")) archives++; });
	const inspector = await openSettings(page);
	const before = Boolean(process.env.AO_U4_RECOVERY_BEFORE);
	const trigger = inspector.getByRole("button", { name: before ? "Retry stopping this session" : "Archive session", exact: true });
	await trigger.click();
	const dialog = page.getByRole("dialog", { name: before ? "Stop this session" : "Are you sure you want to archive this session?" });
	await expect(dialog).toBeVisible();
	if (!before) {
		await expect(dialog).toContainText(/clean worktree will be removed.*branch kept.*unconfirmed writer.*workspace and launch veto/i);
		await expect(dialog.getByRole("button", { name: "Archive session", exact: true })).toBeVisible();
	}
	await page.screenshot({ path: info.outputPath(before ? "recovery-before.png" : "recovery-archive.png"), fullPage: true });
	await page.keyboard.press("Escape");
	await expect(dialog).toHaveCount(0);
	await expect(trigger).toBeFocused();
	expect(archives).toBe(0);
});

test("failed settings stay visible beside actions with review retry and keep-current choices @T0", async ({ page }, info) => {
	test.skip(Boolean(process.env.AO_U4_BEFORE), "The original main has no settings controls");
	await page.setViewportSize({ width: 1440, height: 960 });
	const state = await installSettings(page, "chat");
	const inspector = await openSettings(page);
	const message = page.getByLabel("Message the agent", { exact: true });
	await message.fill("Keep this unsent message when settings fail");
	await inspector.getByRole("button", { name: "Edit agent settings" }).click();
	let editor = page.getByRole("dialog", { name: "Change agent settings" });
	await editor.getByRole("button", { name: /^Model:/ }).click();
	await page.getByRole("menuitem", { name: "pending-model", exact: true }).click();
	await editor.getByRole("button", { name: "Apply settings" }).click();
	const composer = page.getByTestId("agent-settings-composer");
	await expect(composer.getByRole("status")).toContainText("pending-model");
	await state.fail();
	const before = Boolean(process.env.AO_U4_FAILURE_BEFORE);
	if (before) {
		await expect(composer.getByRole("status")).toHaveCount(0);
		await expect(page.getByTestId("agent-settings-header").getByRole("alert")).toHaveCount(0);
		await page.screenshot({ path: info.outputPath("failed-before-hidden.png"), fullPage: true });
		return;
	}
	for (const surface of [page.getByTestId("agent-settings-header"), composer, inspector]) {
		await expect(surface.getByRole("alert")).toContainText("AGENT_SETTINGS_FAILED");
		await expect(surface.getByRole("alert")).toContainText("The provider could not apply the requested settings.");
	}
	await expect(composer.getByRole("alert")).toContainText("failed-request-1");
	await expect(inspector.getByRole("group", { name: "Current selection" })).toContainText("committed-model");
	expect(await inspector.getByText("Recent settings changes", { exact: true }).locator("..").getAttribute("open")).toBeNull();
	await expect(message).toHaveText("Keep this unsent message when settings fail");
	await page.screenshot({ path: info.outputPath("failed-visible.png"), fullPage: true });
	await composer.getByRole("button", { name: "Review and retry" }).click();
	editor = page.getByRole("dialog", { name: "Change agent settings" });
	await expect(editor.getByRole("button", { name: /^Model:/ })).toContainText("pending-model");
	expect(state.patches()).toBe(1);
	await page.screenshot({ path: info.outputPath("failed-retry-review.png"), fullPage: true });
	await editor.getByRole("button", { name: "Apply settings" }).click();
	await expect.poll(() => state.patches()).toBe(2);
	expect(state.requests[1].settings).toEqual(state.requests[0].settings);
	await state.fail();
	await expect(composer.getByRole("alert")).toContainText("failed-request-2");
	await composer.getByRole("button", { name: "Keep current settings" }).click();
	for (const surface of [page.getByTestId("agent-settings-header"), composer, inspector]) await expect(surface.getByRole("alert")).toHaveCount(0);
	expect(state.patches()).toBe(2);
	await expect(message).toHaveText("Keep this unsent message when settings fail");
	await page.screenshot({ path: info.outputPath("failed-dismissed.png"), fullPage: true });
});

test("composer waits for acceptance and preserves a rejected choice for retry @T0", async ({ page }, info) => {
	test.skip(Boolean(process.env.AO_U4_BEFORE), "The original main capture predates these settings controls");
	await page.setViewportSize({ width: 1440, height: 960 });
	const state = await installSettings(page, "chat");
	state.holdChoice();
	await openSettings(page);
	const message = page.getByLabel("Message the agent", { exact: true });
	await message.fill("Do not lose this draft while the picker waits");
	const picker = page.getByRole("group", { name: "Turn settings", exact: true });
	const trigger = picker.locator('[data-shortcut-target="model-picker"]');
	await trigger.click();
	await page.getByRole("menuitem", { name: /^Model/ }).first().hover();
	await page.getByRole("menu").getByText("pending-model", { exact: true }).last().click();
	await page.keyboard.press("Escape");
	await page.keyboard.press("Escape");
	await expect(page.getByRole("menu")).toHaveCount(0);
	await expect.poll(() => state.legacyRequests.length).toBe(1);
	const before = Boolean(process.env.AO_U4_SUBMISSION_BEFORE);
	if (before) {
		await expect(trigger).toContainText("pending-model");
		await page.screenshot({ path: info.outputPath("submission-before-optimistic.png"), fullPage: true });
		state.releaseChoice(true);
		return;
	}
	await expect(trigger).toContainText("committed-model");
	await expect(page.getByTestId("agent-settings-header")).toContainText("Current selection: committed-model");
	await expect(page.getByTestId("agent-settings-composer").getByRole("status")).toHaveCount(0);
	await expect(page.getByTestId("agent-settings-submission")).toContainText("Submitting settings; not accepted yet");
	await expect(page.getByTestId("agent-settings-submission")).not.toContainText("pending-model");
	await page.screenshot({ path: info.outputPath("submission-waiting.png"), fullPage: true });
	state.releaseChoice(true);
	const rejected = page.getByTestId("agent-settings-submission");
	await expect(rejected).toContainText("Settings choice was not accepted");
	await expect(rejected).toContainText("legacy-choice-request");
	await expect(rejected).toContainText("pending-model");
	await expect(trigger).toContainText("committed-model");
	await expect(message).toHaveText("Do not lose this draft while the picker waits");
	await page.screenshot({ path: info.outputPath("submission-rejected.png"), fullPage: true });
	state.releaseChoice(false);
	await rejected.getByRole("button", { name: "Retry sending" }).click();
	await expect.poll(() => state.legacyRequests.length).toBe(2);
	await expect(page.getByTestId("agent-settings-composer").getByRole("status")).toContainText("pending-model");
	await expect(message).toHaveText("Do not lose this draft while the picker waits");
	await page.screenshot({ path: info.outputPath("submission-accepted.png"), fullPage: true });
});

for (const timing of ["deferred", "immediate"] as const) {
test(`idle ${timing} applied settings synchronize the composer through native SSE CDC @T0`, async ({ page }, info) => {
	test.skip(Boolean(process.env.AO_U4_BEFORE), "The original main has no settings controls");
	const peers = new Set<ServerResponse>();
	const server = createServer((request, response) => {
		if (!request.url?.startsWith("/api/v1/events")) { response.writeHead(404).end(); return; }
		response.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", "Access-Control-Allow-Origin": "*" });
		response.flushHeaders();
		response.write("event: cursor\ndata: 0\n\n");
		peers.add(response); request.on("close", () => peers.delete(response));
	});
	await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", resolve); });
	try {
		const address = server.address();
		if (!address || typeof address === "string") throw new Error("Missing isolated SSE listener");
		const state = await installSettings(page, "chat", "codex", { idle: true, immediateApplied: timing === "immediate", realCdcPort: address.port });
		await page.setViewportSize({ width: 1440, height: 960 });
		const inspector = await openSettings(page);
		await expect.poll(() => peers.size).toBe(1);
		await expect.poll(state.conversationReads).toBeGreaterThanOrEqual(2);
		const readsInitial = state.conversationReads();
		const picker = page.getByRole("group", { name: "Turn settings", exact: true }).locator('[data-shortcut-target="model-picker"]');
		await expect(picker).toContainText("committed-model");
		const message = page.getByLabel("Message the agent", { exact: true });
		await message.fill("Preserve this draft across real CDC");
		await inspector.getByRole("button", { name: "Edit agent settings" }).click();
		const editor = page.getByRole("dialog", { name: "Change agent settings" });
		await editor.getByRole("button", { name: /^Model:/ }).click();
		await page.getByRole("menuitem", { name: "pending-model", exact: true }).click();
		await editor.getByRole("button", { name: "Apply settings" }).click();
		await expect(editor).toHaveCount(0);
		const before = Boolean(process.env.AO_U4_CDC_BEFORE || (process.env.AO_U4_IMMEDIATE_BEFORE && timing === "immediate"));
		if (timing === "immediate") {
			await expect(page.getByTestId("agent-settings-header")).toContainText("Current selection: pending-model");
			await expect(picker).toContainText(before ? "committed-model" : "pending-model");
			if (!before) expect(state.conversationReads()).toBeGreaterThan(readsInitial);
		} else {
			await expect(page.getByTestId("agent-settings-composer").getByRole("status")).toContainText("pending-model");
			await expect(picker).toContainText("committed-model");
		}
		const readsBefore = state.conversationReads();
		if (timing === "deferred") await state.apply();
		const settingsBeforeCdc = state.settingsReads();
		const frame = { seq: 1, projectId: "fake-proj", type: "session_updated", sessionId, payload: { id: sessionId } };
		for (const response of peers) response.write(`id: 1\nevent: session_updated\ndata: ${JSON.stringify(frame)}\n\n`);
		await expect.poll(state.settingsReads).toBeGreaterThan(settingsBeforeCdc);
		await expect(page.getByTestId("agent-settings-header")).toContainText("Current selection: pending-model");
		await expect(inspector.getByRole("group", { name: "Current selection" })).toContainText("pending-model");
		if (before) {
			await expect(picker).toContainText("committed-model");
			expect(state.conversationReads()).toBe(readsBefore);
		} else {
			await expect(picker).toContainText("pending-model");
			if (timing === "deferred") expect(state.conversationReads()).toBeGreaterThan(readsBefore);
			else expect(state.conversationReads()).toBeGreaterThan(readsInitial);
		}
		await expect(message).toHaveText("Preserve this draft across real CDC");
		await page.screenshot({ path: info.outputPath(`${timing}-${before ? "cdc-before-stale" : "cdc-synchronized"}.png`), fullPage: true });
		await mkdir(info.outputDir, { recursive: true });
		await writeFile(info.outputPath("native-cdc-receipt.json"), JSON.stringify({ timing, readsInitial, readsAfterMutation: readsBefore, settingsBeforeCdc, settingsAfterCdc: state.settingsReads(), source: "native EventSource over real HTTP SSE; remaining API/provider data are fake", peers: peers.size, frame, readsBefore, readsAfter: state.conversationReads(), header: await page.getByTestId("agent-settings-header").innerText(), picker: await picker.innerText() }, null, 2));
	} finally {
		for (const response of peers) response.end();
		server.closeAllConnections();
		await new Promise<void>((resolve) => server.close(() => resolve()));
	}
});
}
