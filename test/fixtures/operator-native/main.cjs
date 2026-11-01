const { app, BrowserWindow, Menu, dialog } = require("electron");
const { spawn } = require("node:child_process");
const { randomBytes } = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");
const { connectOperatorConsent } = require("./operator-consent.cjs");
const { privateRuntimeHandoff } = require("./app-diagnostics-link.cjs");

// A real native core fixture, not a renderer mock or an automated user gesture.
const modeIndex = process.argv.findIndex((arg) => arg === "--c0-owned" || arg === "--c0-attach");
if (modeIndex < 0 || !process.argv[modeIndex + 1]) throw new Error("fixture mode and private root required");
const root = process.argv[modeIndex + 1];
const owned = process.argv[modeIndex] === "--c0-owned";
const stateRoot = path.join(root, ".ao");
fs.mkdirSync(stateRoot, { recursive: true, mode: 0o700 });
app.setPath("userData", path.join(stateRoot, "electron"));
app.setPath("crashDumps", path.join(stateRoot, "crashes"));
let daemon;
let link;
let polling;
let token;
let stopping = false;
if (owned) {
	token = randomBytes(32).toString("base64url");
	const log = fs.openSync(path.join(stateRoot, "daemon.log"), "a", 0o600);
	daemon = spawn(path.join(process.resourcesPath, "daemon", "ao"), ["-test.run=^TestC0NativeFixtureDaemon$", "-test.timeout=5m"], {
		env: { ...process.env, C0_NATIVE_CHILD: "1", C0_NATIVE_ROOT: root, C0_NATIVE_MODE: "owned" },
		stdio: ["pipe", log, log],
	});
	fs.closeSync(log);
	daemon.stdin.on("error", () => undefined);
	daemon.stdin.end(privateRuntimeHandoff(randomBytes(32).toString("base64url"), randomBytes(32).toString("base64url"), token));
}
process.on("SIGTERM", () => app.quit());
app.on("before-quit", (event) => {
	if (daemon && daemon.exitCode === null && !stopping) {
		event.preventDefault();
		stopping = true;
		daemon.once("exit", () => app.quit());
		daemon.kill("SIGTERM");
	}
});
app.on("will-quit", () => {
	clearInterval(polling);
	link?.dispose();
	daemon?.kill("SIGTERM");
});
app.whenReady().then(() => {
	const window = new BrowserWindow({ width: 720, height: 480, title: "C0 native core fixture", webPreferences: { sandbox: true, contextIsolation: true, nodeIntegration: false } });
	window.once("closed", () => link?.dispose());
	window.loadURL("data:text/html," + encodeURIComponent('<meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src \'unsafe-inline\'"><style>body{font:18px system-ui;padding:40px;background:#181818;color:#eee}p{max-width:520px;line-height:1.6}</style><h1>C0 native core fixture</h1><p>Isolated data. No provider calls. Use the native Operator confirmation menu to review the fixed test plan. This window never confirms automatically.</p>'));
	Menu.setApplicationMenu(Menu.buildFromTemplate([{ label: "Operator confirmation", submenu: [{ label: "Review fixed fixture plan…", accelerator: "CommandOrControl+Alt+Shift+O", click: () => { void link?.reviewNext(); } }] }]));
	polling = setInterval(() => {
		if (link) return;
		try {
			const run = JSON.parse(fs.readFileSync(path.join(stateRoot, "running.json"), "utf8"));
			if (typeof run.address !== "string") return;
			link = connectOperatorConsent({ address: run.address, ownedToken: token, zh: () => true, show: (options) => dialog.showMessageBox(window, options) });
		} catch { /* The real fixture daemon is still migrating its private DB. */ }
	}, 100);
});
