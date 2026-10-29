import { expect, it, vi, afterEach } from "vitest";
import { downloadDocument } from "./document-download";
const mocks = vi.hoisted(() => ({
	resolve: vi.fn(),
	read: vi.fn(),
	get: vi.fn(),
}));
vi.mock("./document-read", () => ({
	resolveDocument: mocks.resolve,
	readDocumentContent: mocks.read,
}));
vi.mock("./api-client", () => ({
	apiClient: { GET: mocks.get },
	apiErrorMessage: () => "error",
}));
afterEach(() => vi.restoreAllMocks());
it("downloads original artifact bytes with the existing cap and exact filename, never an interactive HTML route", async () => {
	const target = {
		kind: "artifact",
		projectId: "p",
		sessionId: "s",
		path: "Unicode 空白.svg",
	} as const;
	mocks.resolve.mockResolvedValue({ target, fingerprint: "v1" });
	mocks.read.mockResolvedValue(
		new TextEncoder().encode('<svg onload="x()"/>').buffer,
	);
	const create = vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:owned");
	vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
	let filename = "";
	vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
		this: HTMLAnchorElement,
	) {
		filename = this.download;
		expect(this.href).toBe("blob:owned");
	});
	await downloadDocument({ kind: "local", identity: "local" }, target);
	expect(mocks.read).toHaveBeenCalledWith(
		{ kind: "local", identity: "local" },
		target,
		50 * 1024 * 1024,
		"v1",
	);
	expect(filename).toBe(target.path);
	expect(create.mock.calls[0][0]).toMatchObject({
		type: "application/octet-stream",
	});
});
it("does not download when current source validation fails", async () => {
	mocks.resolve.mockRejectedValue(new Error("missing"));
	mocks.read.mockClear();
	await expect(
		downloadDocument(
			{ kind: "local", identity: "local" },
			{ kind: "shared", projectId: "p", path: "a.md" },
		),
	).rejects.toThrow("missing");
	expect(mocks.read).not.toHaveBeenCalled();
});
