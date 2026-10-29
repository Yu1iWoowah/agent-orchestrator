import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { DocumentBody, documentErrorKey } from "./DocumentBody";
import { DocumentReadingContext } from "../lib/document-reading-context";
import type { DocumentVisit } from "../stores/document-reading-store";
const mocks = vi.hoisted(() => ({ read: vi.fn(), resolve: vi.fn() }));
vi.mock("../lib/document-read", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/document-read")>()),
	readDocumentContent: mocks.read,
	resolveDocument: mocks.resolve,
}));
vi.mock("../lib/navigate-to-session", () => ({
	useNavigateToSession: () => vi.fn(),
}));
const connection = { kind: "local", identity: "local-test" } as const;
const visit: DocumentVisit = {
	document: {
		target: { kind: "shared", projectId: "p", path: "dir/read.md" },
		canonicalUri: "ao://projects/p/shared/dir/read.md",
		localPath: "/registered/dir/read.md",
		size: 10,
		mediaType: "text/markdown",
		fingerprint: "v1",
	},
	position: { scrollTop: 0, zoom: 1 },
	mode: "preview",
	epoch: 1,
	revealFragment: false,
};
beforeEach(() => {
	mocks.read.mockReset();
	mocks.resolve.mockReset();
});
describe("DocumentBody", () => {
	it.each(["missing.png", "https://blocked.invalid/image.png"])("does not label %s as a resource limit because another image failed", async (reference) => {
		mocks.resolve.mockImplementation(async (_connection, target) => {
			if (target.path.endsWith("large.png")) return { ...visit.document, target, size: 11 * 1024 * 1024 };
			throw new Error("missing");
		});
		mocks.read.mockResolvedValue(new TextEncoder().encode(`![Large screenshot](large.png)\n\n![Problem image](${reference})`).buffer);
		const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const tree = <QueryClientProvider client={cache}><DocumentBody readerKey="chat" connection={connection} visit={visit} /></QueryClientProvider>;
		const view = render(tree);
		await screen.findByText(/Resource exceeds the 10 MiB per-file limit/);
		view.rerender(<QueryClientProvider client={cache}><DocumentBody readerKey="chat" connection={connection} visit={{ ...visit, epoch: 2 }} /></QueryClientProvider>);
		await waitFor(() => expect(screen.getByText(/^Problem image/)).not.toHaveTextContent("Resource limit"));
		expect(screen.getByText(/Resource exceeds the 10 MiB per-file limit/)).toBeVisible();
		view.unmount();
		cache.clear();
	});

	it("explains one oversized Markdown image and still displays the next small image", async () => {
		const png = new Uint8Array(24);
		png.set([137, 80, 78, 71, 13, 10, 26, 10]);
		new DataView(png.buffer).setUint32(16, 1);
		new DataView(png.buffer).setUint32(20, 1);
		mocks.resolve.mockImplementation(async (_connection, target) => ({
			...visit.document,
			target,
			size: target.path.endsWith("large.png")
				? 11 * 1024 * 1024
				: png.byteLength,
		}));
		mocks.read.mockImplementation(async (_connection, target) =>
			target.path.endsWith("read.md")
				? new TextEncoder().encode(
						"![Large screenshot](large.png)\n\n![Small diagram](small.png)",
					).buffer
				: png.buffer,
		);
		const create = vi
			.spyOn(URL, "createObjectURL")
			.mockReturnValue("blob:small");
		const cache = new QueryClient({
			defaultOptions: { queries: { retry: false } },
		});
		const view = render(
			<QueryClientProvider client={cache}>
				<DocumentBody readerKey="chat" connection={connection} visit={visit} />
			</QueryClientProvider>,
		);
		await waitFor(() =>
			expect(
				screen.getByRole("img", { name: "Small diagram" }),
			).toHaveAttribute("src", "blob:small"),
		);
		expect(
			screen.getByText(/Resource exceeds the 10 MiB per-file limit/),
		).toBeVisible();
		expect(
			mocks.read.mock.calls.map(([, target]) => target.path),
		).not.toContain("dir/large.png");
		view.unmount();
		cache.clear();
		create.mockRestore();
	});

	it("reapplies a new fragment after a warm-cache reopen without a new content response", async () => {
		mocks.read.mockResolvedValue(
			new TextEncoder().encode("# First\n\n# Second").buffer,
		);
		vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(
			function (this: HTMLElement) {
				return {
					top: this.id === "second" ? 200 : 0,
					left: 0,
					right: 500,
					bottom: 500,
					width: 500,
					height: 500,
					x: 0,
					y: 0,
					toJSON() {},
				};
			},
		);
		const cache = new QueryClient({
			defaultOptions: { queries: { retry: false } },
		});
		const tree = (v: DocumentVisit) => (
			<QueryClientProvider client={cache}>
				<DocumentReadingContext.Provider
					value={{ key: "chat", sessionId: "chat", connection }}
				>
					<DocumentBody readerKey="chat" connection={connection} visit={v} />
				</DocumentReadingContext.Provider>
			</QueryClientProvider>
		);
		const view = render(tree(visit));
		await screen.findByRole("heading", { name: "Second" });
		const root = view.container.querySelector<HTMLElement>(
			"[data-document-scroll]",
		)!;
		root.scrollTop = 0;
		view.rerender(
			tree({
				...visit,
				epoch: 2,
				revealFragment: true,
				document: {
					...visit.document,
					target: { ...visit.document.target, fragment: "second" },
				},
			}),
		);
		await waitFor(() => expect(root.scrollTop).toBe(200));
		expect(mocks.read).toHaveBeenCalledTimes(1);
		view.unmount();
		cache.clear();
		vi.restoreAllMocks();
	});
	it("shows missing shared sources as not created and refuses non-text bytes under a markdown name", async () => {
		expect(documentErrorKey({ code: "DOCUMENT_SOURCE_MISSING" })).toBe(
			"documents.sheet.notCreated",
		);
		mocks.read.mockResolvedValue(new Uint8Array([0, 255]).buffer);
		const cache = new QueryClient();
		render(
			<QueryClientProvider client={cache}>
				<DocumentBody readerKey="chat" connection={connection} visit={visit} />
			</QueryClientProvider>,
		);
		await screen.findByText("This binary file cannot be previewed.");
		expect(screen.queryByRole("heading")).toBeNull();
		cache.clear();
	});
});

it("rejects declared oversized text from metadata before fetching any content bytes", async () => {
	mocks.read.mockClear();
	const cache = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	render(
		<QueryClientProvider client={cache}>
			<DocumentBody
				readerKey="chat"
				connection={connection}
				visit={{
					...visit,
					document: {
						...visit.document,
						size: 2 * 1024 * 1024 + 1,
						mediaType: "text/markdown",
					},
				}}
			/>
		</QueryClientProvider>,
	);
	await screen.findByText(/Preview limits: UTF-8 text 2 MiB/);
	expect(mocks.read).not.toHaveBeenCalled();
	cache.clear();
});

it("restores history position without reapplying the document's retained heading fragment", async () => {
	mocks.read.mockResolvedValue(
		new TextEncoder().encode("# First\n\n## Second").buffer,
	);
	const cache = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	const view = render(
		<QueryClientProvider client={cache}>
			<DocumentBody
				readerKey="chat"
				connection={connection}
				visit={{
					...visit,
					document: {
						...visit.document,
						target: { ...visit.document.target, fragment: "second" },
					},
					position: { scrollTop: 350, zoom: 1 },
					revealFragment: false,
				}}
			/>
		</QueryClientProvider>,
	);
	await screen.findByRole("heading", { name: "Second" });
	await waitFor(() =>
		expect(
			view.container.querySelector<HTMLElement>("[data-document-scroll]")!
				.scrollTop,
		).toBe(350),
	);
	view.unmount();
	cache.clear();
});

it("never reuses local content bytes on a remote connection with the same identity string", async () => {
	mocks.read
		.mockResolvedValueOnce(
			new TextEncoder().encode("# Local fixture bytes").buffer,
		)
		.mockRejectedValue(
			Object.assign(new Error("local only"), { code: "DOCUMENT_LOCAL_ONLY" }),
		);
	const cache = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	const tree = (kind: "local" | "remote") => (
		<QueryClientProvider client={cache}>
			<DocumentBody
				readerKey="chat"
				connection={{ kind, identity: "same-string" }}
				visit={visit}
			/>
		</QueryClientProvider>
	);
	const view = render(tree("local"));
	await screen.findByRole("heading", { name: "Local fixture bytes" });
	view.rerender(tree("remote"));
	await screen.findByText(
		"Document preview is only available on this desktop connection.",
	);
	expect(
		screen.queryByRole("heading", { name: "Local fixture bytes" }),
	).toBeNull();
	view.unmount();
	cache.clear();
});

it.each([
	["read.md", "preview", 350],
	["read.html", "preview", 0],
] as const)(
	"restores %s %s history to its approved position",
	async (path, mode, expected) => {
		mocks.read.mockResolvedValue(
			new TextEncoder().encode(
				path.endsWith("html")
					? "<h1>History document</h1>"
					: "# History document",
			).buffer,
		);
		const cache = new QueryClient({
			defaultOptions: { queries: { retry: false } },
		});
		const current = {
			...visit,
			mode,
			document: {
				...visit.document,
				target: { ...visit.document.target, path },
			},
		};
		const tree = (epoch: number) => (
			<QueryClientProvider client={cache}>
				<DocumentBody
					readerKey="chat"
					connection={connection}
					visit={{ ...current, epoch, position: { scrollTop: 350, zoom: 1 } }}
				/>
			</QueryClientProvider>
		);
		const view = render(tree(1));
		await waitFor(() =>
			expect(
				view.container.querySelector("[data-document-scroll]"),
			).not.toBeNull(),
		);
		const root = view.container.querySelector<HTMLElement>(
			"[data-document-scroll]",
		)!;
		root.scrollTop = 350;
		view.rerender(tree(2));
		await waitFor(() => expect(root.scrollTop).toBe(expected));
		view.unmount();
		cache.clear();
	},
);
