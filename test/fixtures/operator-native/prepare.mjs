import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../..");
const lab = process.argv[2];
if (!lab || !path.isAbsolute(lab)) throw new Error("an absolute isolated lab directory is required");
const require = createRequire(path.join(repo, "frontend/package.json"));
const { build } = require("vite");
const { createPackage } = require("@electron/asar");
const { flipFuses, FuseVersion, FuseV1Options } = require("@electron/fuses");
const directory = path.join(lab, "node_modules/electron/dist");
await fs.mkdir(lab, { recursive: true, mode: 0o700 });
await fs.cp(path.join(repo, "frontend/node_modules/electron/dist"), directory, { recursive: true });
const source = path.join(lab, "source");
await fs.mkdir(source, { recursive: true, mode: 0o700 });
await build({
	configFile: false, logLevel: "warn",
	build: {
		outDir: source, emptyOutDir: true,
		lib: { entry: { "operator-consent": path.join(repo, "frontend/src/main/operator-consent.ts"), "app-diagnostics-link": path.join(repo, "frontend/src/main/app-diagnostics-link.ts") }, formats: ["cjs"], fileName: (_format, entry) => `${entry}.cjs` },
		rollupOptions: { external: (id) => id.startsWith("node:") || id === "electron" },
	},
});
await fs.copyFile(path.join(repo, "test/fixtures/operator-native/main.cjs"), path.join(source, "main.cjs"));
await fs.writeFile(path.join(source, "package.json"), JSON.stringify({ name: "c0-native-core-fixture", version: "1.0.0", main: "main.cjs" }));
await createPackage(source, path.join(directory, "resources/app.asar"));
const executable = path.join(directory, "electron");
await flipFuses(executable, { version: FuseVersion.V1, [FuseV1Options.RunAsNode]: false, [FuseV1Options.EnableNodeCliInspectArguments]: false, [FuseV1Options.EnableEmbeddedAsarIntegrityValidation]: true, [FuseV1Options.OnlyLoadAppFromAsar]: true });
await fs.copyFile(executable, path.join(directory, "agent-orchestrator"));
await fs.mkdir(path.join(directory, "resources/daemon"), { recursive: true, mode: 0o700 });
console.log(`Native fixture: ${executable}`);
console.log(`Compile operatornative tests to: ${path.join(directory, "resources/daemon/ao")}`);
