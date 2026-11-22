// @vitest-environment node
import { EventEmitter } from "node:events";
import { readFileSync } from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { BaseUpdater } from "electron-updater/out/BaseUpdater.js";
import { NsisUpdater } from "electron-updater/out/NsisUpdater.js";
import ts from "typescript";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { parseRunFile } from "../shared/daemon-discovery";
import { createDesktopQuitController } from "./desktop-quit";
import { DesktopShutdownTimeout, requestDesktopShutdown } from "./desktop-shutdown";
import { ProtectedSideShutdownRefusal, showProtectedSideShutdownRefusal } from "./protected-side-quit";
import { showUpdateFailure } from "./update-failure";

const source = ts.createSourceFile("main.ts", readFileSync(new URL("../main.ts", import.meta.url), "utf8"), ts.ScriptTarget.Latest, true);
const setupCalls = new Set(["setUpdateRestartFailureHandler", "setUpdateShutdownAdmission"]);
const quitFunctions = new Set([
	"shutdownDaemonForDesktopQuit", "relaunchAO", "confirmDesktopQuitDrafts", "cancelDesktopQuitIntent",
	"promptUpdateFailure", "promptProtectedSideShutdown",
]);
const quitVariables = new Set([
	"relaunchAfterFailedHandoff", "updateFailurePrompt", "updateShutdownInFlight", "updateQuitAfterFailedHandoff",
	"protectedSideShutdownPrompt", "updateQuitDeadlineAttempt",
]);
const quitSource = source.statements.filter((node) => {
	if (ts.isFunctionDeclaration(node)) return quitFunctions.has(node.name?.text ?? "");
	if (ts.isVariableStatement(node)) return node.declarationList.declarations.every(({ name }) =>
		ts.isIdentifier(name) && quitVariables.has(name.text));
	if (!ts.isExpressionStatement(node) || !ts.isCallExpression(node.expression)) return false;
	const { expression, arguments: [event] } = node.expression;
	const call = expression.getText(source);
	return setupCalls.has(call) || (ts.isStringLiteral(event) && (
		(call === "app.on" && event.text === "before-quit") ||
		(call === "nativeAutoUpdater.on" && event.text === "before-quit-for-update")
	));
}).map((node) => node.getText(source)).join("\n");
const quitHandler = ts.transpileModule(quitSource, { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText;

function setup() {
	const exit = vi.fn();
	const nativeAutoUpdater = new EventEmitter();
	const app = Object.assign(new EventEmitter(), {
		quit() {
			const preventDefault = vi.fn();
			app.emit("before-quit", { preventDefault });
			if (!preventDefault.mock.calls.length) exit();
		},
		relaunch: vi.fn(),
	});
	let handoffFailed = false;
	let updateRestartRequested = false;
	const registerAdmission = vi.fn<(admit: (signal: AbortSignal) => Promise<boolean>) => void>();
	const registerFailure = vi.fn<(fail: (shutdownAccepted: boolean) => void) => void>();
	const desktopQuit = createDesktopQuitController({
		platform: "win32", hasTray: () => true, hasWindow: () => true,
		isUpdateRestartRequested: () => updateRestartRequested,
		closeWindow: vi.fn(), hideDock: vi.fn(), quit: app.quit,
	});
	const fetch = vi.fn(async (_url: string, _init: RequestInit) => new Response(null, { status: 202 }));
	const showMessageBox = vi.fn<(options: { message: string; buttons: string[] }) => Promise<{ response: number }>>();
	const dialog = { showMessageBox, showErrorBox: vi.fn() };
	const openProtectedSides = vi.fn();
	const focusMainWindow = vi.fn();
	const context = vm.createContext({
		app, nativeAutoUpdater, desktopQuit, dialog, focusMainWindow, queueMicrotask, AbortSignal, fetch, path,
		console: { error: vi.fn() }, process: { platform: "win32" },
		DesktopShutdownTimeout, requestDesktopShutdown, parseRunFile, showUpdateFailure, showProtectedSideShutdownRefusal, ProtectedSideShutdownRefusal,
		setUpdateShutdownAdmission: registerAdmission, setUpdateRestartFailureHandler: registerFailure,
		isUpdateRestartRequested: () => updateRestartRequested, didUpdateHandoffFail: () => handoffFailed,
		clearUpdateRestartRequest: () => { updateRestartRequested = false; },
		confirmUnsafeChatDraftLeave: () => true, chatDraftDialog: {}, chatDraftRisks: [],
		chatDraftQuitConfirmed: false, chatDraftWindowCloseConfirmed: false, preserveAgentsOnQuit: false,
		daemonProcess: null, daemonStatus: { state: "ready", port: 3001 }, appRunId: "test-run",
		runFilePath: () => "/fixture/running.json",
		readFile: async () => JSON.stringify({ pid: 42, port: 3001, owner: "app", appRunId: "test-run" }),
		readUiSettings: async () => ({ locale: "zh-TW" }), operatorConsentZh: true,
		lifecycleLog: vi.fn(), recordUpdateDiagnostic: vi.fn(), desktopAgentShutdownAccepted: false,
		disposeAppDiagnosticsLink: vi.fn(), cancelDaemonAutoRestart: vi.fn(),
		disposeBrowserRuntimeLink: vi.fn(), disposeBrowserStreamLink: vi.fn(),
		trayController: null, trayLifecycle: { dispose: vi.fn(), openProtectedSides },
		browserQuitRequested: false, browserCleanupComplete: true,
		agentQuitCleanupComplete: false, agentQuitCleanupPromise: null,
	});
	vm.runInContext(quitHandler, context);
	return {
		app, nativeAutoUpdater, dialog, exit, fetch, openProtectedSides, focusMainWindow,
		admitUpdate: async () => {
			const accepted = await registerAdmission.mock.calls[0][0](new AbortController().signal);
			updateRestartRequested = true;
			return accepted;
		},
		failHandoff: () => {
			updateRestartRequested = false;
			handoffFailed = true;
			registerFailure.mock.calls[0][0](true);
		},
	};
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
	vi.restoreAllMocks();
	vi.useRealTimers();
});

it.each(["restart button", "ordinary Quit"] as const)("blocks the Windows installer's queued Quit after spawn fails and allows a new %s", async (action) => {
	const h = setup();
	let answerPrompt!: (result: { response: number }) => void;
	h.dialog.showMessageBox.mockReturnValue(new Promise((resolve) => { answerPrompt = resolve; }));
	const updaterQuit = vi.fn();
	h.nativeAutoUpdater.on("before-quit-for-update", updaterQuit);
	const spawnError = vi.fn(() => h.failHandoff());
	expect(await h.admitUpdate()).toBe(true);
	// Run the pinned NSIS/BaseUpdater methods with only the native app and spawn mocked.
	const quitAndInstall = vm.runInNewContext(`({ ${BaseUpdater.prototype.quitAndInstall} }).quitAndInstall`, {
		setImmediate, require: () => ({ autoUpdater: h.nativeAutoUpdater }),
	});
	quitAndInstall.call({
		app: h.app, autoRunAppAfterInstall: true, _logger: { info: vi.fn() },
		install: BaseUpdater.prototype.install, doInstall: Reflect.get(NsisUpdater.prototype, "doInstall"),
		installerPath: "fixture.exe", downloadedUpdateHelper: { downloadedFileInfo: {} },
		spawnLog: async () => { throw Object.assign(new Error("synthetic NSIS spawn failure"), { code: "EIO" }); },
		dispatchError: spawnError,
	}, false, true);
	await vi.advanceTimersByTimeAsync(0);
	expect(spawnError).toHaveBeenCalledOnce();
	expect(h.dialog.showMessageBox).toHaveBeenCalledWith(expect.objectContaining({ message: "更新失敗，請重新啟動 AO" }));
	expect(updaterQuit).toHaveBeenCalledOnce();
	expect(h.dialog.showMessageBox.mock.invocationCallOrder[0]).toBeLessThan(updaterQuit.mock.invocationCallOrder[0]);
	expect(h.exit).not.toHaveBeenCalled();
	expect(h.app.relaunch).not.toHaveBeenCalled();

	answerPrompt({ response: action === "restart button" ? 0 : 1 });
	if (action === "ordinary Quit") h.app.quit();
	await vi.advanceTimersByTimeAsync(0);
	expect(h.exit).toHaveBeenCalledOnce();
	expect(h.app.relaunch).toHaveBeenCalledTimes(action === "restart button" ? 1 : 0);
	expect(h.fetch).toHaveBeenCalledOnce();
});

function useFakeShutdownDeadline() {
	vi.spyOn(AbortSignal, "timeout").mockImplementation((ms) => {
		const deadline = new AbortController();
		setTimeout(() => deadline.abort(new DOMException("Shutdown request timed out", "TimeoutError")), ms);
		return deadline.signal;
	});
}

function rejectOnAbort(signal: AbortSignal | null | undefined): Promise<never> {
	return new Promise((_, reject) => {
		signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
	});
}

it("waits past three seconds for a protected-side 409 and opens the global entry", async () => {
	useFakeShutdownDeadline();
	const h = setup();
	h.dialog.showMessageBox.mockResolvedValue({ response: 0 });
	h.fetch.mockImplementationOnce((_url, { signal }) => Promise.race([
		rejectOnAbort(signal),
		new Promise<Response>((resolve) => setTimeout(() => resolve(Response.json({
			status: "shutdown_not_settled", reason: "protected_side", entry: "protected_side_chats",
			protectedSide: { sideId: "held-side", reason: "scope_host_alive", orphan: false },
		}, { status: 409 })), 4_000)),
	]));
	h.app.quit();
	await vi.advanceTimersByTimeAsync(3_000);
	expect(h.exit).not.toHaveBeenCalled();
	expect(h.dialog.showMessageBox).not.toHaveBeenCalled();
	await vi.advanceTimersByTimeAsync(1_000);
	expect(h.exit).not.toHaveBeenCalled();
	expect(h.dialog.showMessageBox).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({
		buttons: ["開啟受保護 side", "留在 AO"],
	}));
	expect(h.openProtectedSides).toHaveBeenCalledOnce();
	expect(h.dialog.showErrorBox).not.toHaveBeenCalled();
});

it.each(["headers", "409 body"] as const)("stays open and lets the user retry when shutdown times out waiting for %s", async (pending) => {
	useFakeShutdownDeadline();
	const h = setup();
	h.fetch.mockImplementationOnce(async (_url, { signal }) => {
		if (pending === "headers") return rejectOnAbort(signal);
		const response = new Response(null, { status: 409 });
		vi.spyOn(response, "json").mockImplementation(() => rejectOnAbort(signal));
		return response;
	});
	h.app.quit();
	await vi.advanceTimersByTimeAsync(10_000);
	expect(h.exit).not.toHaveBeenCalled();
	expect(h.dialog.showErrorBox).not.toHaveBeenCalled();
	await vi.advanceTimersByTimeAsync(2_000);
	expect(h.exit).not.toHaveBeenCalled();
	expect(h.openProtectedSides).not.toHaveBeenCalled();
	expect(h.dialog.showErrorBox).toHaveBeenCalledExactlyOnceWith("Could not quit Orchestrator", expect.stringContaining("Try quitting again."));
	expect(h.focusMainWindow).toHaveBeenCalledOnce();
	h.app.quit();
	await vi.advanceTimersByTimeAsync(0);
	expect(h.fetch).toHaveBeenCalledTimes(2);
	expect(h.exit).toHaveBeenCalledOnce();
});

it("keeps an ordinary 409 on the existing Quit path without a protected-side prompt", async () => {
	const h = setup();
	h.fetch.mockResolvedValueOnce(Response.json({ status: "shutdown_not_settled", blockedSession: "worker" }, { status: 409 }));
	h.app.quit();
	await vi.advanceTimersByTimeAsync(0);
	expect(h.exit).toHaveBeenCalledOnce();
	expect(h.dialog.showMessageBox).not.toHaveBeenCalled();
	expect(h.dialog.showErrorBox).not.toHaveBeenCalled();
});
