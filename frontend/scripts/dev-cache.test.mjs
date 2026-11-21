// @vitest-environment node
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { afterEach, describe, test } from "vitest";
import {
	assertIgnoredOutputs,
	cacheKey,
	cloneStrategies,
	cloneTree,
	entriesToPrune,
	hashFiles,
	importClosure,
	lookupEntry,
	nodeModulesDrift,
	overrideDir,
	planDeps,
	planStep,
	relativeImports,
	stableStringify,
	stampIsCurrent,
	storeEntry,
	summarizeDrift,
} from "./dev-cache.mjs";
import { daemonSourceFiles, normalizeGoEnv } from "./dev-prepare.mjs";
import { dependencyFields, depsKeyParts, npmSelection, parseArgs } from "./dev-warm.mjs";

const temps = [];
function tempDir() {
	const dir = mkdtempSync(path.join(tmpdir(), "ao-dev-cache-test-"));
	temps.push(dir);
	return dir;
}
afterEach(() => {
	while (temps.length > 0) rmSync(temps.pop(), { recursive: true, force: true });
});

describe("keying", () => {
	test("stableStringify ignores key order and undefined fields", () => {
		assert.equal(stableStringify({ b: 1, a: { d: [1, { y: 2, x: 1 }], c: undefined } }), stableStringify({ a: { d: [1, { x: 1, y: 2 }] }, b: 1 }));
	});

	test("hashFiles depends on contents and paths, not listing order", () => {
		const files = { "a.go": "package a", "b/c.go": "package c" };
		const read = (file) => {
			const rel = path.relative("/root", file).split(path.sep).join("/");
			if (!(rel in files)) throw Object.assign(new Error("missing"), { code: "ENOENT" });
			return Buffer.from(files[rel]);
		};
		const base = hashFiles("/root", ["a.go", "b/c.go"], read);
		assert.equal(hashFiles("/root", ["b/c.go", "a.go", "a.go"], read), base);
		files["a.go"] = "package a // edited";
		assert.notEqual(hashFiles("/root", ["a.go", "b/c.go"], read), base);
		files["a.go"] = "package a";
		// Deleting an input changes the key instead of silently dropping it.
		assert.notEqual(hashFiles("/root", ["a.go", "b/c.go", "gone.go"], read), base);
	});

	test("cacheKey is deterministic and sensitive to every part", () => {
		const parts = { step: "daemon", platform: "darwin", arch: "arm64", files: "abc", extra: { go: { version: "go1.27.4" } } };
		assert.equal(cacheKey(parts), cacheKey({ ...parts }));
		assert.equal(cacheKey(parts).length, 32);
		assert.notEqual(cacheKey(parts), cacheKey({ ...parts, arch: "x64" }));
		assert.notEqual(cacheKey(parts), cacheKey({ ...parts, extra: { go: { version: "go1.27.5" } } }));
	});

	test("cache directory overrides expand ~ and become absolute", () => {
		assert.equal(overrideDir("~/.ao/x", "/home/me"), path.resolve("/home/me/.ao/x"));
		assert.equal(overrideDir("/abs/cache", "/home/me"), path.resolve("/abs/cache"));
		assert.equal(path.isAbsolute(overrideDir("relative/cache", "/home/me")), true);
		assert.equal(overrideDir("", "/home/me"), null);
	});

	test("normalizeGoEnv makes the go.work path checkout-independent", () => {
		const a = normalizeGoEnv({ GOOS: "darwin", GOWORK: "/w/one/go.work", CGO_ENABLED: "1" }, "/w/one");
		const b = normalizeGoEnv({ GOOS: "darwin", GOWORK: "/w/two/go.work", CGO_ENABLED: "1" }, "/w/two");
		assert.deepEqual(a, b);
		assert.equal(a.GOWORK, "go.work");
		assert.equal(normalizeGoEnv({ GOWORK: "off" }, "/w").GOWORK, "off");
	});

	test("daemon inputs include the selected workspace and installed local replacement sources", () => {
		const root = tempDir();
		const local = path.join(root, "components", "hcg");
		const backend = path.join(root, "backend");
		mkdirSync(backend);
		mkdirSync(local, { recursive: true });
		writeFileSync(path.join(root, "selected.work"), "workspace");
		writeFileSync(path.join(backend, "go.mod"), "backend module");
		writeFileSync(path.join(backend, "main.go"), "package main");
		writeFileSync(path.join(local, "go.mod"), "local replacement");
		writeFileSync(path.join(local, "reader.go"), "package reader");
		writeFileSync(path.join(local, "embedded.rules"), "v1");
		const run = (_command, args) => {
			if (args[0] === "work") return JSON.stringify({ Use: [{ DiskPath: "./backend" }] });
			if (args[0] === "mod") return JSON.stringify({ Replace: [{ New: { Path: "../components/hcg" } }] });
			return [
				{ Standard: true },
				{ Module: { Main: true }, Dir: backend, GoFiles: ["main.go"] },
				{ Module: { Replace: { Path: "../components/hcg" } }, Dir: local, GoFiles: ["reader.go"], EmbedFiles: ["embedded.rules"] },
			].map((pkg) => JSON.stringify(pkg, null, 2)).join("\n");
		};
		const files = daemonSourceFiles(root, "selected.work", { run });
		assert.ok(files.includes(path.join("components", "hcg", "go.mod")));
		assert.ok(files.includes("selected.work.sum"));
		const original = hashFiles(root, files);
		writeFileSync(path.join(local, "reader.go"), "package reader\n// changed");
		assert.notEqual(hashFiles(root, files), original);
		writeFileSync(path.join(local, "reader.go"), "package reader");
		writeFileSync(path.join(local, "embedded.rules"), "v2");
		assert.notEqual(hashFiles(root, files), original);
	});

	test("an out-of-checkout workspace module is rejected before Go lists packages", () => {
		const root = tempDir();
		mkdirSync(path.join(root, "backend"));
		writeFileSync(path.join(root, "go.work"), "workspace");
		writeFileSync(path.join(root, "backend", "go.mod"), "backend module");
		const run = (_command, args) => {
			assert.notEqual(args[0], "list");
			if (args[0] === "work") return JSON.stringify({ Use: [{ DiskPath: "../outside" }] });
			return "{}";
		};
		assert.throws(() => daemonSourceFiles(root, "go.work", { run }), /outside this checkout/);
	});

	test("a vendored workspace cannot reuse a daemon keyed on module sources", () => {
		const root = tempDir();
		mkdirSync(path.join(root, "workspace", "vendor"), { recursive: true });
		writeFileSync(path.join(root, "workspace", "go.work"), "workspace");
		const run = () => assert.fail("must reject the vendor directory before probing Go");
		assert.throws(() => daemonSourceFiles(root, path.join("workspace", "go.work"), { run }), /vendor directory/);
	});

	test.skipIf(process.platform === "win32")("dev:warm dry run preserves checkout bytes when Go would add workspace checksums", ({ skip }) => {
		const scratch = realpathSync(tempDir());
		const toolEnv = { ...process.env, GOWORK: "off", GOTOOLCHAIN: "local", GOFLAGS: "", GOENV: "off" };
		for (const command of ["go", "zip"]) {
			if (spawnSync(command, [command === "go" ? "version" : "-v"], { cwd: scratch, env: toolEnv }).error?.code === "ENOENT") skip();
		}
		const repo = path.join(scratch, "repo");
		const zipRoot = path.join(scratch, "archive");
		const dependency = "example.com/probe";
		const version = "v1.0.0";
		const archiveModule = path.join(zipRoot, `${dependency}@${version}`);
		const proxy = path.join(scratch, "proxy");
		const versions = path.join(proxy, dependency, "@v");
		const write = (file, contents) => {
			mkdirSync(path.dirname(file), { recursive: true });
			writeFileSync(file, contents);
		};
		const run = (command, args, cwd, env) => {
			const result = spawnSync(command, args, { cwd, env, encoding: "utf8", windowsHide: true });
			if (result.error) throw result.error;
			assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
			return result.stdout;
		};
		const mod = `module ${dependency}\n\ngo 1.22.0\n`;
		write(path.join(archiveModule, "go.mod"), mod);
		write(path.join(archiveModule, "probe.go"), "package probe\n");
		write(path.join(versions, `${version}.mod`), mod);
		write(path.join(versions, `${version}.info`), JSON.stringify({ Version: version, Time: "2024-01-01T00:00:00Z" }));
		run("zip", ["-q", "-r", path.join(versions, `${version}.zip`), `${dependency}@${version}`], zipRoot);
		const goEnv = {
			...toolEnv,
			GOMODCACHE: path.join(scratch, "modcache"), GOPROXY: pathToFileURL(proxy).href,
			GOSUMDB: "off", GOPRIVATE: "", GONOPROXY: "none", GONOSUMDB: "*",
		};
		run("go", ["mod", "download", `${dependency}@${version}`], scratch, { ...goEnv, GOFLAGS: "-modcacherw" });
		goEnv.GOPROXY = "off";
		const work = path.join(repo, "go.work");
		write(work, "go 1.22.0\n\nuse ./backend\n");
		write(`${work}.sum`, "");
		write(path.join(repo, "backend/go.mod"), `module example.com/backend\n\ngo 1.22.0\n\nrequire ${dependency} ${version}\n`);
		for (const command of ["ao", "ao-chat-host", "ao-mcp"]) {
			write(path.join(repo, `backend/cmd/${command}/main.go`), `package main\nimport _ "${dependency}"\nfunc main() {}\n`);
		}
		for (const file of ["package.json", "package-lock.json"]) write(path.join(repo, "frontend", file), "{}\n");
		run("git", ["init", "-q"], repo);
		run("git", ["add", "."], repo);
		run("git", ["-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture"], repo);
		const tracked = run("git", ["ls-files", "-z"], repo).split("\0").filter(Boolean);
		const before = tracked.map((file) => readFileSync(path.join(repo, file)));
		const env = { ...goEnv, GOWORK: work, AO_DEV_BUILD_CACHE: "1", AO_DEV_DAEMON_REUSE: "" };
		const warm = fileURLToPath(new URL("./dev-warm.mjs", import.meta.url));
		const warmArgs = [warm, "--worktree", repo, "--dry-run"];
		const incomplete = run(process.execPath, warmArgs, repo, env);
		assert.match(incomplete, /daemon\s+build\s+uncached: could not hash inputs/);
		assert.deepEqual(tracked.map((file) => readFileSync(path.join(repo, file))), before);
		assert.equal(run("git", ["status", "--porcelain"], repo), "");
		run("go", ["list", "-mod=readonly", "-deps", "./cmd/ao"], path.join(repo, "backend"), env);
		const completeSum = readFileSync(`${work}.sum`, "utf8");
		assert.match(completeSum, /example\.com\/probe v1\.0\.0 h1:/);
		const complete = run(process.execPath, warmArgs, repo, env);
		assert.match(complete, /daemon\s+build\s+no cached output for inputs [a-f0-9]{12}/);
		assert.equal(readFileSync(`${work}.sum`, "utf8"), completeSum);
	}, 15_000);
});

describe("import closure", () => {
	test("relativeImports finds static, side-effect, re-export and dynamic imports only", () => {
		const source = [
			'import { a } from "./a";',
			'import "./side.css?inline";',
			'export { b } from "../shared/b.js";',
			'const c = await import("./c");',
			'import React from "react";',
		].join("\n");
		assert.deepEqual(relativeImports(source), ["./a", "./side.css", "../shared/b.js", "./c"]);
		assert.deepEqual(relativeImports('import{s}from"../shared/s.js";export*from"./all";'), ["../shared/s.js", "./all"]);
	});

	test("follows relative imports across folders and reports what it cannot resolve", () => {
		const sources = {
			"src/ext/background.ts": 'import { m } from "../main/matcher";\nimport { s } from "../shared/sanitize.js";\nimport { x } from "./missing";',
			"src/main/matcher.ts": 'import { u } from "./util/index";',
			"src/main/util/index.ts": "export const u = 1;",
			"src/shared/sanitize.ts": "export const s = 1;",
		};
		const closure = importClosure("/repo", ["src/ext/background.ts"], {
			isFile: (file) => path.relative("/repo", file).split(path.sep).join("/") in sources,
			read: (file) => sources[path.relative("/repo", file).split(path.sep).join("/")],
		});
		assert.deepEqual(closure.files, ["src/ext/background.ts", "src/main/matcher.ts", "src/main/util/index.ts", "src/shared/sanitize.ts"]);
		assert.deepEqual(closure.unresolved, ["src/ext/background.ts -> ./missing"]);
	});

	test("never resolves above the root", () => {
		const closure = importClosure("/repo", ["a.ts"], { isFile: () => true, read: () => 'import "../outside";' });
		assert.deepEqual(closure.unresolved, ["a.ts -> ../outside"]);
	});
});

describe("cloning", () => {
	test("every platform ends with a plain copy", () => {
		assert.deepEqual(cloneStrategies("darwin"), ["clonefile", "cp-clone", "copy"]);
		assert.deepEqual(cloneStrategies("linux"), ["cp-reflink", "copy"]);
		assert.deepEqual(cloneStrategies("win32"), ["copy"]);
	});

	test("falls back strategy by strategy and cleans partial output", () => {
		const root = tempDir();
		const src = path.join(root, "src");
		mkdirSync(src);
		writeFileSync(path.join(src, "f"), "x");
		const dst = path.join(root, "out", "dst");
		const attempts = [];
		const run = (command, args) => {
			if (command === "xcode-select") return { status: 0 };
			attempts.push(command === "cp" ? `cp ${args[0]}` : "clonefile");
			mkdirSync(dst, { recursive: true }); // partial output a failed attempt leaves behind
			return { status: 1, stderr: "not supported" };
		};
		const method = cloneTree(src, dst, { platform: "darwin", run, copy: cpSync });
		assert.equal(method, "copy");
		assert.deepEqual(attempts, ["clonefile", "cp -c"]);
		assert.equal(readFileSync(path.join(dst, "f"), "utf8"), "x");
	});

	test("refuses to overwrite an existing destination", () => {
		const root = tempDir();
		mkdirSync(path.join(root, "dst"));
		assert.throws(() => cloneTree(root, path.join(root, "dst"), { platform: "win32" }), /refusing to clone over/);
	});
});

describe("cache entries", () => {
	test("store then lookup round-trips, omitting per-checkout caches", () => {
		const root = tempDir();
		const src = path.join(root, "node_modules");
		mkdirSync(path.join(src, ".vite", "deps"), { recursive: true });
		mkdirSync(path.join(src, "react"), { recursive: true });
		writeFileSync(path.join(src, "react", "index.js"), "react");
		writeFileSync(path.join(root, "package-lock.json"), "{}");
		const cache = path.join(root, "cache");
		const copy = (from, to) => cpSync(from, to, { recursive: true });
		assert.equal(lookupEntry(cache, "node_modules", "k1"), null);
		storeEntry(cache, "node_modules", "k1", src, {}, { clone: copy, omit: [".vite"], files: { "package-lock.json": path.join(root, "package-lock.json") } });
		const output = lookupEntry(cache, "node_modules", "k1");
		assert.equal(readFileSync(path.join(output, "react", "index.js"), "utf8"), "react");
		assert.equal(existsSync(path.join(output, ".vite")), false);
		assert.equal(readFileSync(path.join(cache, "node_modules", "k1", "package-lock.json"), "utf8"), "{}");
		// A second store of the same key keeps the first entry untouched.
		writeFileSync(path.join(src, "react", "index.js"), "changed");
		storeEntry(cache, "node_modules", "k1", src, {}, { clone: copy });
		assert.equal(readFileSync(path.join(output, "react", "index.js"), "utf8"), "react");
	});

	test("never replaces an entry another checkout completed during the store", () => {
		const root = tempDir();
		const src = path.join(root, "src");
		mkdirSync(src);
		writeFileSync(path.join(src, "f"), "mine");
		const cache = path.join(root, "cache");
		const final = path.join(cache, "daemon", "k");
		const racingClone = (from, to) => {
			cpSync(from, to, { recursive: true });
			// The other checkout's rename lands while this clone runs.
			mkdirSync(path.join(final, "output"), { recursive: true });
			writeFileSync(path.join(final, "output", "f"), "theirs");
			writeFileSync(path.join(final, "meta.json"), "{}");
		};
		assert.equal(storeEntry(cache, "daemon", "k", src, {}, { clone: racingClone }), final);
		assert.equal(readFileSync(path.join(final, "output", "f"), "utf8"), "theirs");
		assert.deepEqual(readdirSync(path.join(cache, "daemon")), ["k"]);
	});

	test("prunes all but the newest entries and only stale temporaries", () => {
		const now = 10 * 60 * 60 * 1000;
		const entries = [
			{ name: "old", mtimeMs: 1 },
			{ name: "new", mtimeMs: 5 },
			{ name: "mid", mtimeMs: 3 },
			{ name: ".tmp-crashed", mtimeMs: 0 },
			{ name: ".tmp-active", mtimeMs: now - 1000 },
		];
		assert.deepEqual(entriesToPrune(entries, 2, now), ["old", ".tmp-crashed"]);
	});
});

describe("decisions", () => {
	test("stamps must match key and sentinel fingerprint", () => {
		const fingerprint = [["ao", 10, 1000]];
		assert.equal(stampIsCurrent({ key: "k", fingerprint }, "k", [["ao", 10, 1000]]), true);
		assert.equal(stampIsCurrent({ key: "k", fingerprint }, "other", fingerprint), false);
		assert.equal(stampIsCurrent({ key: "k", fingerprint }, "k", [["ao", 11, 1000]]), false);
		assert.equal(stampIsCurrent({ key: "k", fingerprint }, "k", null), false);
		assert.equal(stampIsCurrent(undefined, "k", fingerprint), false);
	});

	test("planStep never trusts an unkeyed step", () => {
		assert.equal(planStep({ key: null, stampCurrent: true, cachedOutput: "/c" }), "build");
		assert.equal(planStep({ key: "k", stampCurrent: true, cachedOutput: null }), "skip");
		assert.equal(planStep({ key: "k", stampCurrent: false, cachedOutput: "/c" }), "restore");
		assert.equal(planStep({ key: "k", stampCurrent: false, cachedOutput: null }), "build");
	});

	test("planDeps clones only an identical lockfile and leaves foreign installs alone", () => {
		const base = { hasNodeModules: false, installedKey: null, key: "k", storeHit: true, lockfilesIdentical: true, cloneSupported: true };
		assert.equal(planDeps(base).action, "clone");
		assert.equal(planDeps({ ...base, lockfilesIdentical: false }).action, "install");
		assert.equal(planDeps({ ...base, storeHit: false }).action, "install");
		assert.equal(planDeps({ ...base, cloneSupported: false }).action, "install");
		assert.equal(planDeps({ ...base, hasNodeModules: true, installedKey: "k" }).action, "keep");
		assert.equal(planDeps({ ...base, hasNodeModules: true, installedKey: null }).action, "foreign");
		assert.equal(planDeps({ ...base, hasNodeModules: true, installedKey: "stale" }).action, "clone");
		assert.equal(planDeps({ ...base, hasNodeModules: true, installedKey: null, drift: true }).action, "clone");
		// A stamped install that drifted is replaced too.
		assert.equal(planDeps({ ...base, hasNodeModules: true, installedKey: "k", drift: true }).action, "clone");
	});

	test("nodeModulesDrift reports missing and mismatched packages, not optional ones", () => {
		const f = path.join(path.sep, "f");
		const files = {
			[path.join(f, "package-lock.json")]: {
				packages: {
					"": { name: "app" },
					"node_modules/a": { version: "1.0.0" },
					"node_modules/b": { version: "2.0.0" },
					"node_modules/c": { version: "1.0.0" },
					"node_modules/fsevents": { version: "2.3.3", optional: true },
					"node_modules/a/node_modules/bundled": { version: "1.0.0", inBundle: true },
					"node_modules/a/node_modules/d": { version: "3.0.0" },
				},
			},
			[path.join(f, "node_modules", ".package-lock.json")]: {
				packages: {
					"node_modules/a": { version: "1.0.0" },
					"node_modules/b": { version: "1.9.0" },
					"node_modules/a/node_modules/d": { version: "3.0.0" },
				},
			},
		};
		const read = (file) => files[file] ?? null;
		const exists = () => true;
		assert.deepEqual(nodeModulesDrift(f, { read, hasDir: () => true, exists }), ["b 1.9.0 (lockfile 2.0.0)", "c missing"]);
		assert.deepEqual(nodeModulesDrift(f, { read, hasDir: () => false, exists }), ["node_modules is missing"]);
		// A required package deleted by hand while npm's record still lists it.
		const gone = path.join(f, "node_modules", "a", "node_modules", "d");
		assert.deepEqual(nodeModulesDrift(f, { read, hasDir: () => true, exists: (dir) => dir !== gone }), ["b 1.9.0 (lockfile 2.0.0)", "c missing", "d missing"]);
		// Without npm's hidden lockfile there is nothing to compare, so no drift.
		assert.deepEqual(nodeModulesDrift(f, { read: (file) => (file.includes(".package-lock") ? null : read(file)), hasDir: () => true, exists }), []);
	});

	test("summarizeDrift caps the list", () => {
		assert.equal(summarizeDrift(["a", "b"]), "a, b");
		assert.equal(summarizeDrift(["a", "b", "c", "d", "e"]), "a, b, c, and 2 more");
	});

	test("assertIgnoredOutputs rejects unignored or tracked targets", () => {
		const fakeGit = ({ ignored, tracked }) => (command, args) => {
			if (args.includes("check-ignore")) return { status: ignored ? 0 : 1 };
			return { status: 0, stdout: tracked ? "frontend/daemon/ao\0" : "" };
		};
		assert.doesNotThrow(() => assertIgnoredOutputs("/repo", ["frontend/daemon"], fakeGit({ ignored: true, tracked: false })));
		assert.throws(() => assertIgnoredOutputs("/repo", ["frontend/daemon"], fakeGit({ ignored: false, tracked: false })), /not ignored/);
		assert.throws(() => assertIgnoredOutputs("/repo", ["frontend/daemon"], fakeGit({ ignored: true, tracked: true })), /tracked files/);
	});
});

describe("dev-warm", () => {
	test("parses its flags and rejects unknown ones", () => {
		assert.deepEqual(parseArgs(["--worktree", "/w", "--dry-run", "--reuse-daemon", "/a/ao"]), {
			worktree: "/w",
			dryRun: true,
			skipPrepare: false,
			replace: false,
			reuseDaemon: "/a/ao",
		});
		assert.throws(() => parseArgs(["--worktree"]), /needs a path/);
		assert.throws(() => parseArgs(["--fast"]), /unknown argument/);
	});

	test("the install key follows the lockfile and dependencies, not scripts", () => {
		const packageJSON = { scripts: { dev: "a" }, dependencies: { react: "^19" }, devDependencies: { vite: "^7" } };
		const parts = { lockfile: Buffer.from("lock"), packageJSON, linkedPackageJSONs: {}, node: "v24.1.0", npm: "11.0.0", platform: "darwin", arch: "arm64" };
		const key = cacheKey(depsKeyParts(parts));
		assert.deepEqual(dependencyFields(packageJSON), { dependencies: { react: "^19" }, devDependencies: { vite: "^7" } });
		assert.equal(cacheKey(depsKeyParts({ ...parts, packageJSON: { ...packageJSON, scripts: { dev: "b" } } })), key);
		assert.notEqual(cacheKey(depsKeyParts({ ...parts, lockfile: Buffer.from("lock2") })), key);
		assert.notEqual(cacheKey(depsKeyParts({ ...parts, node: "v24.2.0" })), key);
		assert.notEqual(cacheKey(depsKeyParts({ ...parts, selection: npmSelection({ npm_config_omit: "dev" }) })), key);
		assert.deepEqual(npmSelection({ PATH: "/bin" }), {});
		assert.notEqual(cacheKey(depsKeyParts({ ...parts, linkedPackageJSONs: { "../packages/ui": { dependencies: { a: "1" } } } })), key);
	});
});
