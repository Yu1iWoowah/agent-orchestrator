import { expect, test } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

test.use({ video: "on" });

// Renderer-only: no native provider, retirement, storage, or worker interruption.
const projectId = "handoff-renderer";
const sessionId = `${projectId}-orchestrator`;
const operationId = "8439e9e9-9a4e-49d9-9010-329ff72ccbc2";

const cases = [
	{ locale: "en", refusal: false },
	{ locale: "en", refusal: true },
	{ locale: "zh-TW", refusal: false },
	{ locale: "zh-TW", refusal: true },
] as const;

for (const { locale, refusal } of cases) {
	test(`AO handoff never sends a provider message (${locale}, ${refusal ? "refused" : "accepted"}) @T0`, async ({ page }, testInfo) => {
		await installFakeAgent(page, { projectId, projectName: "Handoff renderer", workers: [] });
		await page.addInitScript((locale) => {
			window.ao!.uiSettings.get = async () => ({ locale, soundNotificationsEnabled: false, terminalShell: { kind: "auto" } });
			const fake = window.__aoFakeAgent!;
			const snapshot = fake.snapshot;
			fake.snapshot = () => snapshot().map((value) => {
				const project = value as { sessions: Array<Record<string, unknown>> };
				return { ...project, orchestratorAgent: "claude-code", sessions: project.sessions.map(session => ({ ...session, mode: "chat", provider: "claude-code", status: "idle", activity: { state: "idle" }, terminalHandleId: undefined })) };
			});
		}, locale);
		const requests: Array<{ operationKey: string; sourceSessionId: string }> = [];
		let providerSends = 0;
		await page.route("http://127.0.0.1:8080/api/v1/**", async route => {
			const path = new URL(route.request().url()).pathname;
			if (route.request().method() === "POST" && path.endsWith("/orchestrator/replacements")) {
				requests.push(route.request().postDataJSON());
				if (refusal) {
					await route.fulfill({ status: 409, json: { error: { code: "ORCHESTRATOR_HANDOFF_SOURCE_UNSUPPORTED", message: "This source is not supported." } } });
				} else await route.fulfill({ status: 202, json: { operation: { id: operationId, operationKey: requests.at(-1)?.operationKey, projectId, sourceSessionId: sessionId, phase: "accepted" } } });
			} else if (path.endsWith("/conversation") && route.request().method() === "GET") {
				await route.fulfill({ json: { conversationId: "handoff-renderer-conversation", sessionId, harness: "claude-code", mode: "chat", controller: "ready", latestSequence: 0, oldestSequence: 0, hasMoreBefore: false, turns: [], messages: [], activities: [], settings: {} } });
			} else if (path === `/api/v1/projects/${projectId}`) {
				await route.fulfill({ json: { project: { id: projectId, agent: "claude-code", config: { orchestrator: { agent: "claude-code" } } } } });
			} else if (path.endsWith("/conversation/skills")) {
				await route.fulfill({ json: { skills: [] } });
			} else if (path.endsWith("/conversation/models")) {
				await route.fulfill({ json: { models: [], selected: {} } });
			} else if (path.endsWith("/agents/chat/permission-modes")) {
				await route.fulfill({ json: { agents: [{ agent: "claude-code", modes: ["default"] }] } });
			} else {
				if (route.request().method() === "POST" && /\/(messages|send|steer|interrupt)$/.test(path)) providerSends++;
				await route.fulfill({ json: { status: "ok", agents: [], plans: [], installers: [], jobs: [], files: [], truncated: false } });
			}
		});
		await page.goto(`/#/projects/${projectId}/sessions/${sessionId}`);
		const field = page.getByRole("combobox", { name: "Message the agent" });
		await expect(field).toBeVisible();
		await field.fill("/hand");
		await expect(page.getByRole("option").filter({ hasText: locale === "en" ? "Prepare a handoff" : "交接" })).toBeVisible();
		await page.screenshot({ path: testInfo.outputPath("handoff-menu.png") });
		await field.fill("/handoff");
		await field.press("Enter");
		await expect.poll(() => requests.length).toBe(1);
		if (refusal) {
			await expect(page.getByRole("alert")).toContainText("This source is not supported.");
			await expect(field).toContainText("/handoff");
			await page.screenshot({ path: testInfo.outputPath("handoff-refused.png") });
		} else {
			await expect(page.getByRole("status").filter({ hasText: locale === "en" ? "Handoff request accepted." : "已受理交接請求。" })).toContainText(`ao orchestrator replacement get ${operationId} --json`);
			await expect(field).toBeEmpty();
			await page.screenshot({ path: testInfo.outputPath("handoff-accepted.png") });
		}
		expect(requests[0]?.sourceSessionId).toBe(sessionId);
		expect(requests[0]?.operationKey).toMatch(/^[0-9a-f-]{36}$/);
		expect(providerSends).toBe(0);
	});

}
