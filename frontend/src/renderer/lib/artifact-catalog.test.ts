import { describe, expect, it, vi } from "vitest";
import type { components } from "../../api/schema";
import { readProjectCatalog, readArtifactCatalogs } from "./artifact-catalog";
const mocks = vi.hoisted(() => ({ fetch: vi.fn() }));
vi.mock("./document-read", async (original) => ({
	...(await original<typeof import("./document-read")>()),
	fetchDocumentCatalog: mocks.fetch,
}));
const connection = { kind: "local", identity: "daemon" } as const;
const item = (path: string): components["schemas"]["DocumentCatalogItem"] => ({
	name: path,
	path,
	size: 1,
	fingerprint: path,
	source: "shared",
	archived: false,
	target: { kind: "shared", projectId: "p", path },
});
const page = (path: string, nextCursor?: string) => ({
	items: [item(path)],
	errors: [],
	incomplete: false,
	revision: "v1",
	nextCursor,
});
describe("bounded catalog metadata pagination", () => {
	it.each([1205, 4101])("reads all %s entries beyond the previous source caps", async (count) => {
		mocks.fetch.mockReset().mockImplementation(async (_connection, _project, query) => {
			const offset = Number(query.cursor ?? 0),
				end = Math.min(offset + 200, count);
			return {
				items: Array.from({ length: end - offset }, (_, index) => item(`dir/${offset + index}.md`)),
				errors: [],
				incomplete: false,
				revision: "same",
				nextCursor: end < count ? String(end) : undefined,
			};
		});
		const result = await readProjectCatalog(connection, "p", new AbortController().signal);
		expect(result.items).toHaveLength(count);
		expect(result.items.at(-1)?.path).toBe(`dir/${count - 1}.md`);
		expect(mocks.fetch).toHaveBeenCalledTimes(Math.ceil(count / 200));
	});
	it("limits concurrent projects and still publishes healthy results", async () => {
		let active = 0,
			peak = 0;
		mocks.fetch.mockReset().mockImplementation(async (_connection, projectId) => {
			peak = Math.max(peak, ++active);
			await new Promise<void>((resolve) => queueMicrotask(resolve));
			active--;
			return { ...page("ok.md"), items: [{ ...item("ok.md"), target: { kind: "shared", projectId, path: "ok.md" } }] };
		});
		const result = await readArtifactCatalogs(
			connection,
			Array.from({ length: 12 }, (_, index) => `project-${index}`),
			new AbortController().signal,
		);
		expect(result).toHaveLength(12);
		expect(result.every((project) => project.items.length === 1 && !project.loading)).toBe(true);
		expect(peak).toBeLessThanOrEqual(3);
	});
	it("never requests a later page after cancellation", async () => {
		mocks.fetch.mockReset().mockResolvedValue(page("ok.md", "next"));
		const abort = new AbortController();
		await expect(readProjectCatalog(connection, "p", abort.signal, () => abort.abort())).rejects.toMatchObject({
			name: "AbortError",
		});
		expect(mocks.fetch).toHaveBeenCalledTimes(1);
	});
	it("rejects mismatched source ownership without adding that row", async () => {
		mocks.fetch
			.mockReset()
			.mockResolvedValue({
				...page("wrong.md"),
				items: [{ ...item("wrong.md"), target: { kind: "shared", projectId: "other", path: "wrong.md" } }],
			});
		const result = await readProjectCatalog(connection, "p", new AbortController().signal);
		expect(result.items).toEqual([]);
		expect(result.error).toMatchObject({ code: "DOCUMENT_CATALOG_INVALID" });
	});
	it("reads every page beyond the old cap and publishes immutable progress", async () => {
		mocks.fetch
			.mockReset()
			.mockImplementation(async (_connection, _project, query) =>
				query.cursor === undefined
					? page("first.md", "c1")
					: query.cursor === "c1"
						? page("beyond-1000.md", "c2")
						: page("last.md"),
			);
		const progress: number[] = [];
		const result = await readProjectCatalog(connection, "p", new AbortController().signal, (value) =>
			progress.push(value.items.length),
		);
		expect(result.items.map((entry) => entry.path)).toEqual(["first.md", "beyond-1000.md", "last.md"]);
		expect(result.incomplete).toBe(false);
		expect(progress).toEqual([1, 2, 3]);
		expect(mocks.fetch.mock.calls[0][2]).toMatchObject({ source: "all", archive: "all", limit: 200 });
	});
	it("retains successful rows and requires refresh when a later cursor expires", async () => {
		mocks.fetch
			.mockReset()
			.mockResolvedValueOnce(page("first.md", "cursor"))
			.mockRejectedValueOnce(Object.assign(new Error("changed"), { code: "DOCUMENT_CATALOG_CHANGED" }));
		const result = await readProjectCatalog(connection, "p", new AbortController().signal);
		expect(result.items.map((entry) => entry.path)).toEqual(["first.md"]);
		expect(result.incomplete).toBe(true);
		expect(result.error).toMatchObject({ code: "DOCUMENT_CATALOG_CHANGED" });
	});
	it("does not loop forever or mix rows from another revision", async () => {
		mocks.fetch
			.mockReset()
			.mockResolvedValueOnce(page("first.md", "same"))
			.mockResolvedValue({ ...page("new.md", "same"), revision: "v2" });
		const result = await readProjectCatalog(connection, "p", new AbortController().signal);
		expect(result.items.map((entry) => entry.path)).toEqual(["first.md"]);
		expect(result.incomplete).toBe(true);
		expect(mocks.fetch).toHaveBeenCalledTimes(2);
	});
	it("keeps a healthy project alongside failed and uninitialized sources", async () => {
		mocks.fetch.mockReset().mockImplementation(async (_connection, id) => {
			if (id === "bad") throw new Error("offline");
			return {
				...page("ok.md"),
				items: [{ ...item("ok.md"), target: { kind: "shared", projectId: id, path: "ok.md" } }],
				errors: [{ source: "shared", code: "DOCUMENT_SOURCE_MISSING" }],
				incomplete: true,
			};
		});
		const result = await readArtifactCatalogs(connection, ["good", "bad"], new AbortController().signal);
		expect(result.find((project) => project.projectId === "good")?.items).toHaveLength(1);
		expect(result.find((project) => project.projectId === "bad")).toMatchObject({
			incomplete: true,
			items: [],
			error: expect.any(Error),
		});
	});
	it("never routes a remote connection through the local catalog", async () => {
		mocks.fetch.mockReset();
		await expect(
			readArtifactCatalogs({ kind: "remote", identity: "other" }, ["p"], new AbortController().signal),
		).rejects.toThrow();
		expect(mocks.fetch).not.toHaveBeenCalled();
	});
});
