// @vitest-environment node
import { createHash } from "node:crypto";
import { openSync, closeSync, writeSync, promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import forkContract from "../../../cloud/internal/httpapi/testdata/artifact-share-desktop.json";
import { artifactShareUrl } from "../shared/cloud-share";

const token = { value: "token-1" as string | null };
vi.mock("electron", () => ({ ipcMain: {} }));
vi.mock("./cloud-auth", () => ({ getCloudAccessToken: vi.fn(async () => token.value) }));

import { createCloudCpProxy } from "./cloud-cp-proxy";
import {
	CloudShareError,
	resolveShareFile,
	shareFileFingerprint,
	snapshotShareFile,
	statShareFile,
	listShareLinks,
	SHARE_TIMEOUTS,
	shareLinksAvailable,
	shareCloudSessionArtifact,
	uploadShare,
	type CloudShareApi,
} from "./cloud-share";

const sha = (value: Uint8Array | string) => createHash("sha256").update(value).digest("hex");

let dataDir: string;
let artifacts: string;

beforeEach(async () => {
	dataDir = await fs.mkdtemp(path.join(os.tmpdir(), "ao-share-"));
	artifacts = path.join(dataDir, "artifacts", "sess-1");
	await fs.mkdir(path.join(artifacts, "docs"), { recursive: true });
	await fs.mkdir(path.join(artifacts, ".versions", "blobs"), { recursive: true });
	await fs.writeFile(path.join(artifacts, "docs", "report.html"), "<h1>report</h1>");
	await fs.writeFile(path.join(artifacts, ".versions", "blobs", "old"), "old");
	await fs.writeFile(path.join(dataDir, "secret.txt"), "outside");
	token.value = "token-1";
});

afterEach(async () => {
	await fs.rm(dataDir, { recursive: true, force: true });
});

describe("resolveShareFile", () => {
	it("resolves a file inside the session's artifacts folder", async () => {
		const file = await resolveShareFile(dataDir, "sess-1", "docs/report.html");
		expect(file).toMatchObject({ name: "report.html", relativePath: "docs/report.html", size: 15 });
		expect(file.realPath).toBe(await fs.realpath(path.join(artifacts, "docs", "report.html")));
	});

	it("refuses anything outside it, the version history and non-files", async () => {
		await fs.symlink(path.join(dataDir, "secret.txt"), path.join(artifacts, "link.txt"));
		await fs.symlink(dataDir, path.join(artifacts, "up"));
		for (const [sessionId, relPath] of [
			["sess-1", "../../secret.txt"],
			["sess-1", path.join(dataDir, "secret.txt")],
			["sess-1", "link.txt"],
			["sess-1", "up/secret.txt"],
			["sess-1", ".versions/blobs/old"],
			["sess-1", "docs"],
			["sess-1", ""],
			["sess-1", "missing.html"],
			["..", "sess-1/docs/report.html"],
			["sess/1", "docs/report.html"],
			[42, "docs/report.html"],
		] as const) {
			await expect(resolveShareFile(dataDir, sessionId, relPath), `${String(sessionId)} ${relPath}`).rejects.toBeInstanceOf(CloudShareError);
		}
	});
});

describe("share file fingerprint", () => {
	it("names a file by its size and modification time, without reading it", async () => {
		await fs.writeFile(path.join(artifacts, "config.yaml"), "token: one");
		const file = await resolveShareFile(dataDir, "sess-1", "config.yaml");
		const read = vi.spyOn(fs, "readFile");
		const fingerprint = await statShareFile(file);
		const stat = await fs.stat(file.realPath);
		expect(fingerprint).toBe(shareFileFingerprint(stat));
		expect(fingerprint).toBe(`${stat.size}:${Math.trunc(stat.mtimeMs)}`);
		expect(read).not.toHaveBeenCalled();
		read.mockRestore();
		// The snapshot of the same file has it; a rewritten file does not.
		const snapshot = await snapshotShareFile(file, path.join(dataDir, "snapshots"));
		expect(snapshot.fingerprint).toBe(fingerprint);
		const later = new Date(Date.now() + 60_000);
		await fs.writeFile(path.join(artifacts, "config.yaml"), "token: two, longer");
		await fs.utimes(path.join(artifacts, "config.yaml"), later, later);
		expect(await statShareFile(await resolveShareFile(dataDir, "sess-1", "config.yaml"))).not.toBe(fingerprint);
	});
});

describe("snapshotShareFile", () => {
	it("copies the file privately, hashing it", async () => {
		const file = await resolveShareFile(dataDir, "sess-1", "docs/report.html");
		const snapshot = await snapshotShareFile(file, path.join(dataDir, "snapshots"));
		expect(await fs.readFile(snapshot.path, "utf8")).toBe("<h1>report</h1>");
		expect(snapshot).toMatchObject({ size: 15, sha256: sha("<h1>report</h1>") });
		expect((await fs.stat(snapshot.path)).mode & 0o777).toBe(0o600);
	});

	it("refuses a file swapped for a symlink after it resolved", async () => {
		const file = await resolveShareFile(dataDir, "sess-1", "docs/report.html");
		await fs.rm(file.realPath);
		await fs.symlink(path.join(dataDir, "secret.txt"), file.realPath);
		await expect(snapshotShareFile(file, path.join(dataDir, "snapshots"))).rejects.toThrow();
		expect(await fs.readdir(path.join(dataDir, "snapshots")).catch(() => [])).toEqual([]);
	});

	it("refuses a folder on the way swapped for a symlink after the file resolved", async () => {
		const outside = path.join(dataDir, "outside");
		await fs.mkdir(outside);
		await fs.writeFile(path.join(outside, "report.html"), "private");
		const file = await resolveShareFile(dataDir, "sess-1", "docs/report.html");
		await fs.rename(path.join(artifacts, "docs"), path.join(artifacts, "docs-old"));
		await fs.symlink(outside, path.join(artifacts, "docs"));
		await expect(snapshotShareFile(file, path.join(dataDir, "snapshots"))).rejects.toThrow("changed");
		await expect(statShareFile(file)).rejects.toThrow("changed");
		expect(await fs.readdir(path.join(dataDir, "snapshots")).catch(() => [])).toEqual([]);
	});

	it("refuses a file rewritten while it is copied, though its size stays the same", async () => {
		// 2 MiB, last written long ago: a write during the copy moves its times.
		const big = path.join(artifacts, "big.bin");
		await fs.writeFile(big, Buffer.alloc(2 << 20, 1));
		const past = new Date("2020-01-01T00:00:00Z");
		await fs.utimes(big, past, past);
		const file = await resolveShareFile(dataDir, "sess-1", "big.bin");
		const inspected = await statShareFile(file);
		let rewritten = false;
		const snapshot = snapshotShareFile(file, path.join(dataDir, "snapshots"), {
			// After the first MiB is copied, an agent rewrites the second one.
			onProgress: () => {
				if (rewritten) return;
				rewritten = true;
				const handle = openSync(big, "r+");
				writeSync(handle, Buffer.alloc(1 << 20, 2), 0, 1 << 20, 1 << 20);
				closeSync(handle);
			},
		});
		await expect(snapshot).rejects.toMatchObject({ code: "source_changed" });
		expect(rewritten).toBe(true);
		expect(await fs.readdir(path.join(dataDir, "snapshots"))).toEqual([]);
		// Left alone, the same file copies, and names its new version.
		const again = await snapshotShareFile(await resolveShareFile(dataDir, "sess-1", "big.bin"), path.join(dataDir, "snapshots"));
		expect(again.fingerprint).not.toBe(inspected);
		expect(again.size).toBe(2 << 20);
	});

	it("reports progress and stops when cancelled", async () => {
		await fs.writeFile(path.join(artifacts, "big.bin"), Buffer.alloc(3 << 20, 1));
		const file = await resolveShareFile(dataDir, "sess-1", "big.bin");
		const copied: number[] = [];
		await snapshotShareFile(file, path.join(dataDir, "snapshots"), { onProgress: (bytes) => copied.push(bytes) });
		expect(copied).toEqual([1 << 20, 2 << 20, 3 << 20]);

		const controller = new AbortController();
		const stopped = snapshotShareFile(file, path.join(dataDir, "cancelled"), {
			signal: controller.signal,
			onProgress: () => controller.abort(),
		});
		await expect(stopped).rejects.toMatchObject({ code: "cancelled" });
		expect(await fs.readdir(path.join(dataDir, "cancelled"))).toEqual([]);
	});
});

interface Call {
	url: string;
	method: string;
	headers: Record<string, string>;
	body?: string | Uint8Array;
}

function fakeControlPlane(partSize: number, options: { failPart?: number; finalizeError?: { status: number; code: string }; files?: boolean; failFile?: string } = {}) {
	const calls: Call[] = [];
	const stored = new Map<number, Uint8Array>();
	// An included file's bytes, by the path it is served at.
	const files = new Map<string, string>();
	const api: CloudShareApi = {
		controlPlaneUrl: async () => "https://cp.test", getToken: async () => token.value,
		fetch: vi.fn(async (url: string, init: RequestInit) => {
			const headers = Object.fromEntries(new Headers(init.headers).entries());
			const body = init.body as string | Uint8Array | undefined;
			calls.push({ url, method: init.method ?? "GET", headers, body });
			const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
			const { pathname } = new URL(url);
			if (pathname.endsWith("/me/artifact-shares/uploads")) {
				const { size } = JSON.parse(body as string) as { size: number };
				return json({ uploadToken: "tok", partSize, partCount: Math.max(1, Math.ceil(size / partSize)) }, 201);
			}
			// A control plane that predates included files has no such routes.
			if (pathname.endsWith("/uploads/files") && options.files) {
				const request = JSON.parse(body as string) as { uploadToken: string; path: string; size: number };
				if (request.uploadToken !== "tok") return json({ code: "artifact_share_upload_invalid", message: "bad" }, 400);
				if (request.path === "") return json({ code: "invalid_request", message: "path" }, 400);
				return json({ uploadToken: `file:${request.path}`, partSize, partCount: Math.max(1, Math.ceil(request.size / partSize)) }, 201);
			}
			if (pathname.endsWith("/uploads/files/complete") && options.files) {
				const { uploadToken } = JSON.parse(body as string) as { uploadToken: string };
				return json({ fileToken: `ft:${uploadToken.slice("file:".length)}`, path: uploadToken.slice("file:".length), expiresAt: "2026-11-16T00:00:00Z" });
			}
			if (pathname.endsWith("/uploads/parts")) {
				const { uploadToken, parts } = JSON.parse(body as string) as { uploadToken: string; parts: Array<{ partNumber: number }> };
				const prefix = uploadToken === "tok" ? "" : `/file/${encodeURIComponent(uploadToken.slice("file:".length))}`;
				return json({ parts: parts.map(({ partNumber }) => ({ partNumber, method: "PUT", url: `https://s3.test${prefix}/part/${partNumber}`, headers: { "Content-Length": "4", Host: "s3.test", "X-Amz-Checksum-Sha256": "sig" }, size: 0 })) });
			}
			if (pathname.startsWith("/file/")) {
				const filePath = decodeURIComponent(pathname.split("/")[2]);
				if (filePath === options.failFile) return new Response("", { status: 400 });
				files.set(filePath, (files.get(filePath) ?? "") + Buffer.from(body as Uint8Array).toString());
				return new Response("", { headers: { etag: `"file-etag"` } });
			}
			if (pathname.startsWith("/part/")) {
				const number = Number(pathname.split("/").pop());
				if (number === options.failPart) return new Response("", { status: 400 });
				stored.set(number, new Uint8Array(body as Uint8Array));
				return new Response("", { headers: { etag: `"etag-${number}"` } });
			}
			if (pathname.endsWith("/finalize")) {
				if (options.finalizeError) return json({ code: options.finalizeError.code, message: "refused" }, options.finalizeError.status);
				return json({ share: { id: "s-1", slug: "abc" }, replaced: false }, 201);
			}
			if (pathname.endsWith("/uploads/abort")) return new Response(null, { status: 204 });
			if (pathname.endsWith("/me/artifact-shares")) return json({ shares: [], limit: 3, used: 1 });
			return new Response("", { status: 404 });
		}),
	};
	return { api, calls, stored, files };
}

async function snapshotOf(content: string, name = "upload.txt") {
	await fs.writeFile(path.join(artifacts, name), content);
	const file = await resolveShareFile(dataDir, "sess-1", name);
	return snapshotShareFile(file, path.join(dataDir, "snapshots"));
}

// A connection that dropped answers nothing: the request only ends when its
// signal aborts.
function silentControlPlane(): CloudShareApi {
	return {
		controlPlaneUrl: async () => "https://cp.test", getToken: async () => "token-1",
		fetch: vi.fn((_url: string, init: RequestInit) => new Promise<Response>((_resolve, reject) => {
			const fail = () => reject(init.signal?.reason ?? new DOMException("Aborted", "AbortError"));
			// Like fetch, a request cancelled before it starts fails at once.
			if (init.signal?.aborted) return fail();
			init.signal?.addEventListener("abort", fail, { once: true });
		})),
	};
}

describe("share destination", () => {
	it("uses the configured base path and refuses redirects on token-bearing requests", async () => {
		const api: CloudShareApi = {
			controlPlaneUrl: async () => "https://cp.test/prefix/",
			getToken: vi.fn(async () => "token-1"),
			fetch: vi.fn(async () => Response.json({ shares: [] })),
		};
		await listShareLinks(api, "https://CP.test:443/prefix");
		expect(api.fetch).toHaveBeenCalledWith("https://cp.test/prefix/api/cloud/v1/me/artifact-shares", expect.objectContaining({ redirect: "error" }));
	});

	it("rechecks the destination before falling back to an inline upload", async () => {
		let configured: string | null = "https://cp.test";
		const api: CloudShareApi = {
			controlPlaneUrl: async () => configured,
			getToken: vi.fn(async () => "token-1"),
			fetch: vi.fn(async () => {
				configured = null;
				return new Response("", { status: 404 });
			}),
		};
		await expect(uploadShare(api, { baseUrl: "https://cp.test", snapshot: await snapshotOf("v1"), name: "upload.txt", project: "p", feature: "f" }, () => undefined)).rejects.toThrow(/configured/);
		expect(api.getToken).toHaveBeenCalledTimes(1);
		expect(api.fetch).toHaveBeenCalledTimes(1);
	});
});

describe("share request time limits", () => {
	const saved = { ...SHARE_TIMEOUTS };
	beforeEach(() => Object.assign(SHARE_TIMEOUTS, { api: 30, probe: 30, transfer: 30, publish: 30 }));
	afterEach(() => Object.assign(SHARE_TIMEOUTS, saved));

	it("fails a request that never answers instead of waiting for good", async () => {
		await expect(listShareLinks(silentControlPlane(), "https://cp.test")).rejects.toMatchObject({
			code: "timeout",
			message: "Orchestrator Cloud didn't answer in time. Check your connection and try again.",
		});
		await expect(shareLinksAvailable(silentControlPlane(), "https://cp.test")).resolves.toBeNull();
	});

	it("bounds a stalled destination lookup before reading a token", async () => {
		const api: CloudShareApi = { controlPlaneUrl: () => new Promise(() => undefined), getToken: vi.fn(), fetch: vi.fn() };
		await expect(listShareLinks(api, "https://cp.test")).rejects.toMatchObject({ code: "timeout" });
		await expect(shareLinksAvailable(api, "https://cp.test")).resolves.toBeNull();
		expect(api.getToken).not.toHaveBeenCalled();
		expect(api.fetch).not.toHaveBeenCalled();
	});

	it("counts a token refresh that stalls against the request's limit", async () => {
		const fetch = vi.fn();
		const stalledToken: CloudShareApi = { controlPlaneUrl: async () => "https://cp.test", getToken: () => new Promise<string | null>(() => undefined), fetch };
		await expect(listShareLinks(stalledToken, "https://cp.test")).rejects.toMatchObject({ code: "timeout" });
		await expect(shareLinksAvailable(stalledToken, "https://cp.test")).resolves.toBeNull();
		expect(fetch).not.toHaveBeenCalled();
	});

	it("keeps a cancel a cancel, not a timeout", async () => {
		const snapshot = await snapshotOf("0123456789");
		Object.assign(SHARE_TIMEOUTS, { api: 60_000 });
		const controller = new AbortController();
		const upload = uploadShare(silentControlPlane(), { baseUrl: "https://cp.test", snapshot, name: "upload.txt", project: "p", feature: "sess-1" }, () => undefined, controller.signal);
		controller.abort();
		const error = await upload.catch((caught: unknown) => caught);
		expect(error).not.toBeInstanceOf(CloudShareError);
	});

	it("times out an upload part that stops answering", async () => {
		const snapshot = await snapshotOf("0123456789");
		const { api } = fakeControlPlane(4);
		const answer = api.fetch;
		api.fetch = vi.fn((url: string, init: RequestInit) => new URL(url).pathname.startsWith("/part/")
			? silentControlPlane().fetch(url, init)
			: answer(url, init));
		await expect(uploadShare(api, { baseUrl: "https://cp.test", snapshot, name: "upload.txt", project: "p", feature: "sess-1" }, () => undefined))
			.rejects.toMatchObject({ code: "timeout" });
	});
});

describe("uploadShare", () => {
	it("uploads the snapshot in signed parts and finalizes it", async () => {
		const snapshot = await snapshotOf("0123456789");
		const { api, calls, stored } = fakeControlPlane(4);
		const progress: number[] = [];
		const result = await uploadShare(api, { baseUrl: "https://cp.test", snapshot, name: "upload.txt", project: "p", feature: "sess-1" }, (sent) => progress.push(sent));
		expect(result.share.id).toBe("s-1");
		// Parts upload concurrently, so they can arrive in any order.
		expect([...stored.entries()].sort(([a], [b]) => a - b).map(([number, body]) => [number, Buffer.from(body).toString()])).toEqual([[1, "0123"], [2, "4567"], [3, "89"]]);
		expect(progress.at(-1)).toBe(10);

		const start = JSON.parse(calls[0].body as string);
		expect(start).toEqual({ name: "upload.txt", size: 10, sha256: sha("0123456789"), project: "p", feature: "sess-1" });
		const presign = JSON.parse(calls.find((call) => call.url.endsWith("/uploads/parts"))!.body as string);
		expect(presign.parts).toEqual([{ partNumber: 1, sha256: sha("0123") }, { partNumber: 2, sha256: sha("4567") }, { partNumber: 3, sha256: sha("89") }]);
		const finalize = JSON.parse(calls.find((call) => call.url.endsWith("/finalize"))!.body as string);
		expect(finalize.parts.map((part: { partNumber: number; etag: string }) => [part.partNumber, part.etag])).toEqual([[1, '"etag-1"'], [2, '"etag-2"'], [3, '"etag-3"']]);

		// Chromium sets Content-Length and Host itself and refuses a request
		// that names them; the other signed headers go with each part.
		for (const call of calls.filter((candidate) => candidate.url.startsWith("https://s3.test/"))) {
			expect(call.headers).toEqual({ "x-amz-checksum-sha256": "sig" });
		}

		// The token goes only to the control plane, never to storage.
		for (const call of calls) {
			expect(call.headers.authorization === "Bearer token-1").toBe(call.url.startsWith("https://cp.test/api/cloud/v1/"));
		}
	});

	it("updates a link by its ID, naming where the file is now", async () => {
		const snapshot = await snapshotOf("x");
		const { api, calls } = fakeControlPlane(4);
		await uploadShare(api, { baseUrl: "https://cp.test", snapshot, name: "upload.txt", project: "p", feature: "f", shareId: "share-9" }, () => undefined);
		expect(JSON.parse(calls[0].body as string)).toEqual({ name: "upload.txt", size: 1, sha256: sha("x"), project: "p", feature: "f", shareId: "share-9" });
	});

	it("aborts the upload when a part fails or finalize refuses", async () => {
		const snapshot = await snapshotOf("0123456789");
		const failing = fakeControlPlane(4, { failPart: 2 });
		await expect(uploadShare(failing.api, { baseUrl: "https://cp.test", snapshot, name: "upload.txt", project: "p", feature: "f" }, () => undefined)).rejects.toThrow("part 2");
		expect(failing.calls.some((call) => call.url.endsWith("/uploads/abort"))).toBe(true);

		const refused = fakeControlPlane(4, { finalizeError: { status: 409, code: "artifact_share_limit_reached" } });
		await expect(uploadShare(refused.api, { baseUrl: "https://cp.test", snapshot, name: "upload.txt", project: "p", feature: "f" }, () => undefined))
			.rejects.toMatchObject({ code: "artifact_share_limit_reached", status: 409 });
	});

	it("stops every part once one fails, before the upload is aborted", async () => {
		const snapshot = await snapshotOf("0123456789abcdefghij");
		const failing = fakeControlPlane(4, { failPart: 1 });
		await expect(uploadShare(failing.api, { baseUrl: "https://cp.test", snapshot, name: "upload.txt", project: "p", feature: "f" }, () => undefined)).rejects.toThrow("part 1");
		const abort = failing.calls.findIndex((call) => call.url.endsWith("/uploads/abort"));
		expect(abort).toBeGreaterThan(0);
		expect(failing.calls.slice(abort).filter((call) => call.url.startsWith("https://s3.test/"))).toEqual([]);
		// Not every part was sent: the second worker stopped.
		expect(failing.calls.filter((call) => call.url.startsWith("https://s3.test/")).length).toBeLessThan(5);
	});

	it("uploads a page's files under its upload first and lists them when finalizing", async () => {
		const page = await snapshotOf("<img src=a.png>", "page.html");
		const files = [
			{ path: "images/a.png", snapshot: await snapshotOf("aaaaaa", "a.png") },
			{ path: "b.svg", snapshot: await snapshotOf("bb", "b.svg") },
		];
		const control = fakeControlPlane(4, { files: true });
		const progress: number[] = [];
		const result = await uploadShare(control.api, { baseUrl: "https://cp.test", snapshot: page, name: "page.html", project: "p", feature: "f", page: { files } }, (sent) => progress.push(sent));
		expect(result.pageFiles).toEqual({ included: 2, unsupported: false });
		expect(Object.fromEntries(control.files)).toEqual({ "images/a.png": "aaaaaa", "b.svg": "bb" });
		const starts = control.calls.filter((call) => call.url.endsWith("/uploads/files")).map((call) => JSON.parse(call.body as string));
		expect(starts).toEqual([
			{ uploadToken: "tok", path: "images/a.png", size: 6, sha256: sha("aaaaaa") },
			{ uploadToken: "tok", path: "b.svg", size: 2, sha256: sha("bb") },
		]);
		const finalize = JSON.parse(control.calls.find((call) => call.url.endsWith("/finalize"))!.body as string);
		expect(finalize.files).toEqual([{ fileToken: "ft:images/a.png" }, { fileToken: "ft:b.svg" }]);
		// Progress counts the files and the page together.
		expect(progress.at(-1)).toBe(6 + 2 + 15);
	});

	it("clears a page's files with an empty list, once the control plane serves them", async () => {
		const page = await snapshotOf("<p>no images</p>", "page.html");
		const control = fakeControlPlane(4, { files: true });
		const result = await uploadShare(control.api, { baseUrl: "https://cp.test", snapshot: page, name: "page.html", project: "p", feature: "f", page: { files: [] } }, () => undefined);
		expect(result.pageFiles).toEqual({ included: 0, unsupported: false });
		const finalize = JSON.parse(control.calls.find((call) => call.url.endsWith("/finalize"))!.body as string);
		expect(finalize.files).toEqual([]);
	});

	it("publishes a page alone, without a files list, to a control plane that predates them", async () => {
		const page = await snapshotOf("<img src=a.png>", "page.html");
		for (const files of [[], [{ path: "a.png", snapshot: await snapshotOf("aaaa", "a.png") }]]) {
			const control = fakeControlPlane(4);
			const result = await uploadShare(control.api, { baseUrl: "https://cp.test", snapshot: page, name: "page.html", project: "p", feature: "f", page: { files } }, () => undefined);
			expect(result.pageFiles).toEqual({ included: 0, unsupported: true });
			const finalize = JSON.parse(control.calls.find((call) => call.url.endsWith("/finalize"))!.body as string);
			expect(finalize).not.toHaveProperty("files");
		}
	});

	it("leaves the files out of a file that is not a page", async () => {
		const snapshot = await snapshotOf("x");
		const control = fakeControlPlane(4, { files: true });
		const result = await uploadShare(control.api, { baseUrl: "https://cp.test", snapshot, name: "upload.txt", project: "p", feature: "f" }, () => undefined);
		expect(result).not.toHaveProperty("pageFiles");
		expect(control.calls.some((call) => call.url.includes("/uploads/files"))).toBe(false);
	});

	it("aborts a page's file upload and the page's when a file fails", async () => {
		const page = await snapshotOf("<img src=a.png>", "page.html");
		const files = [{ path: "a.png", snapshot: await snapshotOf("aaaa", "a.png") }];
		const control = fakeControlPlane(4, { files: true, failFile: "a.png" });
		await expect(uploadShare(control.api, { baseUrl: "https://cp.test", snapshot: page, name: "page.html", project: "p", feature: "f", page: { files } }, () => undefined)).rejects.toThrow("part 1");
		const aborted = control.calls.filter((call) => call.url.endsWith("/uploads/abort")).map((call) => JSON.parse(call.body as string).uploadToken);
		expect(aborted).toEqual(["file:a.png", "tok"]);
		expect(control.calls.some((call) => call.url.endsWith("/finalize"))).toBe(false);
	});

	it("needs a signed-in user and a safe control plane URL", async () => {
		const { api } = fakeControlPlane(4);
		token.value = null;
		await expect(listShareLinks(api, "https://cp.test")).rejects.toMatchObject({ code: "signed_out" });
		token.value = "token-1";
		await expect(listShareLinks(api, "http://evil.test")).rejects.toThrow("https");
		await expect(listShareLinks(api, "https://cp.test")).resolves.toMatchObject({ limit: 3, used: 1, unlimited: false });
	});
});

describe("fork inline share contract", () => {
	// TestArtifactShareDesktopContract checks these wire responses against the
	// real Go router. Do not invent multipart endpoints in this fixture.
	it("does not infer Cloud publishing from the fork's local share list", async () => {
		const cloud = forkContract.cloudSessionShare;
		const api: CloudShareApi = {
			controlPlaneUrl: async () => "https://cp.test", getToken: async () => "token-1",
			fetch: vi.fn(async (url, init) => {
				expect(init.method).toBe("GET");
				if (new URL(url).pathname === "/api/cloud/v1/me/artifact-shares") return Response.json(forkContract.list);
				expect(new URL(url).pathname).toBe(`/api/cloud/v1/orgs/${cloud.orgId}/sessions/${cloud.sessionId}/artifacts/share`);
				return new Response(cloud.body, { status: cloud.status, headers: { Allow: cloud.allow } });
			}),
		};
		await expect(shareLinksAvailable(api, "https://cp.test")).resolves.toBe(true);
		await expect(shareLinksAvailable(api, "https://cp.test", cloud)).resolves.toBe(false);
		for (const shareId of [undefined, forkContract.publish.share.id]) {
			await expect(shareCloudSessionArtifact(api, "https://cp.test", { ...cloud, path: "upload.txt", sha256: sha("v1"), shareId }))
				.rejects.toMatchObject({ code: "unsupported_sharing" });
		}
	});

	it.each([
		{ status: 405, allow: "POST", available: true },
		{ status: 405, allow: "GET, POST", available: true },
		{ status: 405, allow: "DELETE", available: false },
		{ status: 405, allow: "", available: false },
		{ status: 200, allow: "", available: null },
		{ status: 401, allow: "", available: null },
		{ status: 403, allow: "", available: null },
		{ status: 500, allow: "", available: null },
	])("requires the Cloud route to advertise POST (%j)", async ({ status, allow, available }) => {
		const api: CloudShareApi = { controlPlaneUrl: async () => "https://cp.test", getToken: async () => "token-1", fetch: vi.fn(async () => new Response("", { status, headers: { Allow: allow } })) };
		await expect(shareLinksAvailable(api, "https://cp.test", forkContract.cloudSessionShare)).resolves.toBe(available);
	});

	it("publishes, republishes and copies links using the fork's routes and URL field", async () => {
		const requests: string[] = [];
		const api: CloudShareApi = {
			controlPlaneUrl: async () => "https://cp.test", getToken: async () => "token-1",
			fetch: vi.fn(async (url, init) => {
				const target = new URL(url);
				const route = `${init.method} ${target.pathname}`;
				requests.push(route);
				const headers = new Headers(init.headers);
				expect(headers.get("Authorization")).toBe("Bearer token-1");
				if (route === "POST /api/cloud/v1/me/artifact-shares/uploads") {
					return new Response(forkContract.unsupportedMultipart.body, { status: forkContract.unsupportedMultipart.status });
				}
				if (route === "GET /api/cloud/v1/me/artifact-shares") return Response.json(forkContract.list);
				const updating = init.method === "PUT";
				expect(target.pathname).toBe(`/api/cloud/v1/me/artifact-shares${updating ? `/${forkContract.publish.share.id}` : ""}`);
				expect(init.method).toBe(updating ? "PUT" : "POST");
				expect(target.searchParams.get("name")).toBe("upload.txt");
				expect(target.searchParams.get("project")).toBe("p");
				expect(target.searchParams.get("feature")).toBe("f");
				expect(init.body).toEqual(Buffer.from(updating ? "v2" : "v1"));
				expect(headers.get("X-AO-Artifact-SHA256")).toBe(sha(updating ? "v2" : "v1"));
				expect(headers.get("Content-Type")).toBe("application/octet-stream");
				// Chromium derives these itself from the Buffer body.
				expect(headers.has("Content-Length")).toBe(false);
				expect(headers.has("Host")).toBe(false);
				return Response.json(updating ? forkContract.republish : forkContract.publish, { status: updating ? 200 : 201 });
			}),
		};
		const input = { baseUrl: "https://cp.test", name: "upload.txt", project: "p", feature: "f" };
		expect(await shareLinksAvailable(api, input.baseUrl)).toBe(true);
		const progress = vi.fn();
		const first = await uploadShare(api, { ...input, snapshot: await snapshotOf("v1") }, progress);
		expect(first.replaced).toBe(false);
		expect(first.share.sha256).toBe(forkContract.publish.share.sha256);
		expect(artifactShareUrl(first.share)).toBe(forkContract.publish.share.url);
		const second = await uploadShare(api, { ...input, snapshot: await snapshotOf("v2"), shareId: first.share.id }, progress);
		expect(second.replaced).toBe(true);
		expect(second.share.sha256).toBe(forkContract.republish.share.sha256);
		expect(artifactShareUrl(second.share)).toBe(artifactShareUrl(first.share));
		expect(progress.mock.calls).toEqual([[2], [2]]);
		const listed = await listShareLinks(api, input.baseUrl);
		expect(listed).toMatchObject({ limit: 3, used: 1, unlimited: false, privateLinks: false });
		expect(listed.shares).toEqual([second.share]);
		expect(artifactShareUrl(listed.shares[0])).toBe(forkContract.publish.share.url);
		expect(requests).toEqual([
			"GET /api/cloud/v1/me/artifact-shares",
			"POST /api/cloud/v1/me/artifact-shares/uploads",
			"POST /api/cloud/v1/me/artifact-shares",
			"POST /api/cloud/v1/me/artifact-shares/uploads",
			"GET /api/cloud/v1/me/artifact-shares",
			`PUT /api/cloud/v1/me/artifact-shares/${first.share.id}`,
			"GET /api/cloud/v1/me/artifact-shares",
		]);
	});

	it("refuses inline republish of a restricted link when an update omits access", async () => {
		const fetch = vi.fn<CloudShareApi["fetch"]>()
			.mockResolvedValueOnce(new Response("", { status: 404 }))
			.mockResolvedValueOnce(Response.json({ ...forkContract.list, shares: [{ ...forkContract.publish.share, access: "restricted" }] }));
		const api: CloudShareApi = { controlPlaneUrl: async () => "https://cp.test", getToken: async () => "token-1", fetch };
		await expect(uploadShare(api, { baseUrl: "https://cp.test", snapshot: await snapshotOf("v2"), name: "upload.txt", project: "p", feature: "f", shareId: forkContract.publish.share.id }, () => undefined))
			.rejects.toMatchObject({ code: "unsupported_sharing" });
		expect(fetch.mock.calls.map(([, request]) => request.method)).toEqual(["POST", "GET"]);
	});

	it.each([{ access: "restricted" as const }, { emails: ["reader@example.com"] }])("does not turn a private request into a public inline upload (%j)", async (access) => {
		const api: CloudShareApi = { controlPlaneUrl: async () => "https://cp.test", getToken: async () => "token-1", fetch: vi.fn(async () => new Response("", { status: 405 })) };
		await expect(uploadShare(api, { baseUrl: "https://cp.test", snapshot: await snapshotOf("private"), name: "upload.txt", project: "p", feature: "f", ...access }, () => undefined))
			.rejects.toMatchObject({ code: "unsupported_sharing" });
		expect(api.fetch).toHaveBeenCalledTimes(1);
	});

	it("refuses an oversized inline snapshot before reading or sending its bytes", async () => {
		const api: CloudShareApi = { controlPlaneUrl: async () => "https://cp.test", getToken: async () => "token-1", fetch: vi.fn(async () => new Response("", { status: 404 })) };
		const snapshot = { path: "does-not-exist", size: forkContract.maxBytes + 1, sha256: sha(""), fingerprint: "" };
		await expect(uploadShare(api, { baseUrl: "https://cp.test", snapshot, name: "upload.txt", project: "p", feature: "f" }, () => undefined))
			.rejects.toMatchObject({ code: "artifact_share_too_large" });
		expect(api.fetch).toHaveBeenCalledTimes(1);
	});

	it.each([401, 403, 404, 409, 500])("does not retry a structured upload error as an inline publish (%i)", async (status) => {
		const api: CloudShareApi = { controlPlaneUrl: async () => "https://cp.test", getToken: async () => "token-1", fetch: vi.fn(async () => Response.json({ code: "upload_refused" }, { status })) };
		await expect(uploadShare(api, { baseUrl: "https://cp.test", snapshot: await snapshotOf("v1"), name: "upload.txt", project: "p", feature: "f" }, () => undefined))
			.rejects.toMatchObject({ status });
		expect(api.fetch).toHaveBeenCalledTimes(1);
	});
});

describe("cloud control-plane proxy", () => {
	it("lets the renderer read share links but never change them", async () => {
		const fetchImpl = vi.fn(async () => new Response("{}", { status: 200 }));
		const proxy = createCloudCpProxy(() => dataDir, { controlPlaneUrl: async () => "https://cp.test", fetchImpl });
		const request = (method: string, path: string) => proxy.request({ baseUrl: "https://cp.test", path, method });
		await expect(request("GET", "/api/cloud/v1/me/artifact-shares")).resolves.toMatchObject({ status: 200 });
		for (const [method, path] of [
			["POST", "/api/cloud/v1/me/artifact-shares/uploads"],
			["POST", "/api/cloud/v1/me/artifact-shares/finalize"],
			["DELETE", "/api/cloud/v1/me/artifact-shares/00000000-0000-4000-8000-000000000000"],
			["POST", "/api/cloud/v1/me/artifact-shares"],
			["post", "/api/cloud/v1/me/./artifact-shares/uploads"],
			["POST", "/api/cloud/v1/orgs/../me/artifact-shares/finalize"],
			// A cloud session's file is shared only through the main process too.
			["POST", "/api/cloud/v1/orgs/a1a1a1a1-0000-4000-8000-000000000001/sessions/a1a1a1a1-0000-4000-8000-0000000000a1/artifacts/share"],
			["POST", "/api/cloud/v1/orgs/o/sessions/s/artifacts%2Fshare"],
		]) {
			await expect(request(method, path), `${method} ${path}`).rejects.toThrow("share links can only be changed");
		}
		// Reading a session's files is still allowed.
		await expect(request("GET", "/api/cloud/v1/orgs/o/sessions/s/artifacts")).resolves.toMatchObject({ status: 200 });
		expect(fetchImpl).toHaveBeenCalledTimes(2);
	});
});
