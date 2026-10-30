const { app, BrowserWindow } = require("electron");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const profiles = path.join(os.homedir(), ".ao", "dev", "e2e-settings");
fs.mkdirSync(profiles, { recursive: true });
app.setPath("userData", fs.mkdtempSync(path.join(profiles, "profile-")));
app.whenReady().then(() => {
	new BrowserWindow({
		width: 1280,
		height: 720,
		webPreferences: {
			sandbox: true,
			contextIsolation: true,
			nodeIntegration: false,
			preload: path.join(__dirname, "settings-sandbox-preload.cjs"),
		},
	}).loadURL("about:blank");
});
