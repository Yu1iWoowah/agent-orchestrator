vi.mock("../DocumentCode", () => ({
	DocumentCode: ({ text }: { text: string }) => <pre>{text}</pre>,
}));
import { DocumentReadError } from "../../lib/document-read";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
	act,
	fireEvent,
	render,
	screen,
	waitFor,
} from "@testing-library/react";
import { beforeEach, expect, it, vi } from "vitest";
import { DocumentSheet } from "./DocumentSheet";
import { DocumentReadingContext } from "../../lib/document-reading-context";
import { aoBridge } from "../../lib/bridge";
import { useDocumentReadingStore } from "../../stores/document-reading-store";
import { useConversationFrameStore } from "../../stores/conversation-frame-store";
import { TooltipProvider } from "../ui/tooltip";
const mocks = vi.hoisted(() => ({
	resolve: vi.fn(),
	read: vi.fn(),
	navigate: vi.fn(),
}));
vi.mock("../../lib/document-read", async (original) => ({
	...(await original<typeof import("../../lib/document-read")>()),
	resolveDocument: mocks.resolve,
	readDocumentContent: mocks.read,
}));
vi.mock("../../lib/navigate-to-session", () => ({
	useNavigateToSession: () => mocks.navigate,
}));
vi.mock("../../hooks/useConversationFrameSafeRect", () => ({
	useConversationFrameSafeRect: () => ({
		surface: document.body,
		rect: { left: 10, top: 50, width: 800, height: 500 },
	}),
}));
const reader = {
	key: "current",
	sessionId: "orchestrator",
	connection: { kind: "local", identity: "test" },
} as const;
const target = (path: string) => ({
	kind: "shared" as const,
	projectId: "p",
	path,
});
const meta = (path: string) => ({
	target: target(path),
	localPath: "/managed/" + path,
	canonicalUri: "ao://projects/p/shared/" + path,
	size: 10,
	mediaType: "text/plain",
	fingerprint: path,
});
beforeEach(() => {
	mocks.resolve.mockReset();
	mocks.read
		.mockReset()
		.mockImplementation(
			async (_connection, target) =>
				new TextEncoder().encode("Body " + target.path).buffer,
		);
	mocks.navigate.mockReset();
	useDocumentReadingStore.setState({ sessions: {}, reader: null });
});
function setup() {
	const surface = document.createElement("section");
	document.body.append(surface);
	const cache = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	const view = render(
		<QueryClientProvider client={cache}>
			<TooltipProvider>
				<DocumentReadingContext.Provider value={reader}>
					<DocumentSheet
						surfaceRef={{ current: surface }}
						composerRef={{ current: surface }}
					/>
				</DocumentReadingContext.Provider>
			</TooltipProvider>
		</QueryClientProvider>,
	);
	return { surface, cache, ...view };
}
it("aborts rapid A→B, ignores a late A and preserves Plan preferences and focus restore", async () => {
	let completeA!: (value: ReturnType<typeof meta>) => void;
	let signalA: AbortSignal | undefined;
	mocks.resolve.mockImplementation((_connection, input, signal) => {
		if (input.path === "a.txt") {
			signalA = signal;
			return new Promise((resolve) => {
				completeA = resolve;
			});
		}
		return Promise.resolve(meta(input.path));
	});
	const plan = useConversationFrameStore.getState().sessions;
	const { cache, surface, unmount } = setup();
	const source = document.createElement("button");
	document.body.append(source);
	source.focus();
	act(() =>
		useDocumentReadingStore.getState().open(reader.key, target("a.txt")),
	);
	expect(await screen.findByRole("status")).toHaveTextContent(
		"Loading document…",
	);
	act(() =>
		useDocumentReadingStore.getState().open(reader.key, target("b.txt")),
	);
	await screen.findByText("Body b.txt");
	expect(signalA?.aborted).toBe(true);
	await act(async () => completeA(meta("a.txt")));
	expect(screen.queryByText("Body a.txt")).toBeNull();
	expect(mocks.navigate).not.toHaveBeenCalled();
	expect(useConversationFrameStore.getState().sessions).toEqual(plan);
	fireEvent.keyDown(screen.getByRole("region", { name: "b.txt" }), {
		key: "Escape",
	});
	await waitFor(() => expect(source).toHaveFocus());
	expect(useDocumentReadingStore.getState().sessions.current.foreground).toBe(
		false,
	);
	unmount();
	cache.clear();
	surface.remove();
	source.remove();
});
it("keeps a missing shared root as not created and lets Plan foreground collapse the document", async () => {
	mocks.resolve.mockRejectedValue(
		new DocumentReadError("DOCUMENT_SOURCE_MISSING", "missing"),
	);
	const { surface, cache, unmount } = setup();
	act(() =>
		useDocumentReadingStore.getState().open(reader.key, target("missing.md")),
	);
	await waitFor(() =>
		expect(screen.getByRole("status")).toHaveTextContent("Not created yet"),
	);
	act(() => surface.dispatchEvent(new CustomEvent("ao:frame-foreground")));
	expect(useDocumentReadingStore.getState().sessions.current.foreground).toBe(
		false,
	);
	unmount();
	cache.clear();
	surface.remove();
});

it.each([undefined, "ao://projects/p/shared/missing%2Emd"])(
	"copies only the current unresolved link from the missing-source view (%s)",
	async (originalLink) => {
		mocks.resolve.mockRejectedValue(
			new DocumentReadError("DOCUMENT_SOURCE_MISSING", "missing"),
		);
		const write = vi
			.spyOn(aoBridge.clipboard, "writeText")
			.mockResolvedValue(undefined);
		const { surface, cache, unmount } = setup();
		act(() =>
			useDocumentReadingStore.getState().show(reader.key, meta("previous.md")),
		);
		act(() =>
			useDocumentReadingStore
				.getState()
				.open(reader.key, target("missing.md"), { originalLink }),
		);
		await waitFor(() =>
			expect(screen.getByRole("status")).toHaveTextContent("Not created yet"),
		);
		fireEvent.click(screen.getByRole("button", { name: /^Copy link$/ }));
		await waitFor(() =>
			expect(write).toHaveBeenCalledWith(
				originalLink ?? "ao://projects/p/shared/missing.md",
			),
		);
		write.mockRestore();
		unmount();
		cache.clear();
		surface.remove();
	},
);

it("captures the opener before delayed Sheet autofocus or an unrelated focus change", async () => {
	let complete!: (value: ReturnType<typeof meta>) => void;
	mocks.resolve.mockImplementation(
		() =>
			new Promise((resolve) => {
				complete = resolve;
			}),
	);
	const { surface, cache, unmount } = setup();
	const source = document.createElement("button"),
		other = document.createElement("button");
	document.body.append(source, other);
	source.focus();
	act(() => {
		useDocumentReadingStore.getState().open(reader.key, target("b.txt"));
	});
	await screen.findByRole("status");
	other.focus();
	await act(async () => complete(meta("b.txt")));
	await screen.findByText("Body b.txt");
	fireEvent.keyDown(screen.getByRole("region", { name: "b.txt" }), {
		key: "Escape",
	});
	await waitFor(() => expect(source).toHaveFocus());
	unmount();
	cache.clear();
	surface.remove();
	source.remove();
	other.remove();
});
