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
