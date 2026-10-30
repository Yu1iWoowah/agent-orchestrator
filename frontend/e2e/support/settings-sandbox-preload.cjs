// Test-only proof from the actual renderer process, never an AO bridge.
const { contextBridge } = require("electron");
contextBridge.exposeInMainWorld("__aoSettingsSandboxProof", {
	rendererSandbox: process.sandboxed === true,
	contextIsolated: process.contextIsolated === true,
});
