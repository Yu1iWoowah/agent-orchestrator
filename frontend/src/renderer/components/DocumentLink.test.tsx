import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { AppLink } from "./AppLink";
import { DocumentReadingContext } from "../lib/document-reading-context";
import { useDocumentReadingStore } from "../stores/document-reading-store";
import { TooltipProvider } from "./ui/tooltip";
import { aoBridge } from "../lib/bridge";
const mocks = vi.hoisted(() => ({ resolve: vi.fn(), navigate: vi.fn() }));
vi.mock("../lib/document-read", async (original) => ({
	...(await original<typeof import("../lib/document-read")>()),
	resolveDocument: mocks.resolve,
}));
vi.mock("../lib/navigate-to-session", () => ({
	useNavigateToSession: () => mocks.navigate,
}));
const input = {
	kind: "shared",
	projectId: "next.js",
	path: "Unicode 空白/%2e%2e%2fx%",
} as const;
const uri =
	"ao://projects/next.js/shared/Unicode%20%E7%A9%BA%E7%99%BD/%252e%252e%252fx%25";
const metadata = {
	target: input,
	canonicalUri: uri,
	localPath: "/managed/next%2Ejs/shared/Unicode 空白/%2e%2e%2fx%",
	size: 7,
	mediaType: "text/plain",
	fingerprint: "v1",
};
beforeEach(() => {
	mocks.resolve.mockReset().mockResolvedValue(metadata);
	mocks.navigate.mockReset();
	useDocumentReadingStore.setState({ sessions: {}, reader: null });
});
afterEach(() => vi.restoreAllMocks());
function setup() {
	const cache = new QueryClient({
		defaultOptions: { queries: { retry: false } },
	});
	const view = render(
		<QueryClientProvider client={cache}>
			<TooltipProvider>
				<DocumentReadingContext.Provider
					value={{
						key: "current",
						sessionId: "orchestrator",
						connection: { kind: "local", identity: "daemon" },
					}}
				>
					<AppLink href={uri}>Document</AppLink>
					<AppLink href="ao://sessions/p/s">Session</AppLink>
				</DocumentReadingContext.Provider>
			</TooltipProvider>
		</QueryClientProvider>,
	);
	return { cache, ...view };
}
it("opens the current reader with literal target names and leaves pure session handling alone", () => {
	const { cache } = setup();
	fireEvent.click(screen.getByRole("link", { name: "Document" }));
	expect(
		useDocumentReadingStore.getState().sessions.current.opening?.input,
	).toEqual(input);
	expect(mocks.navigate).not.toHaveBeenCalled();
	expect(screen.getByRole("link", { name: "Session" })).toHaveAttribute(
		"href",
		"ao://sessions/p/s",
	);
	cache.clear();
});
it("shows verified paths on keyboard focus and copies exact backend values after fresh validation", async () => {
	const write = vi
		.spyOn(aoBridge.clipboard, "writeText")
		.mockResolvedValue(undefined);
	Object.defineProperty(navigator, "clipboard", {
		configurable: true,
		value: { writeText: vi.fn() },
	});
	const { cache } = setup();
	const link = screen.getByRole("link", { name: "Document" });
	fireEvent.focus(link);
	await waitFor(() =>
		expect(screen.getAllByText(metadata.localPath).length).toBeGreaterThan(0),
	);
	fireEvent.contextMenu(link);
	await userEvent.click(
		screen.getByRole("menuitem", { name: "Copy file path" }),
	);
	await waitFor(() =>
		expect(write).toHaveBeenLastCalledWith(metadata.localPath),
	);
	fireEvent.contextMenu(link);
	await userEvent.click(screen.getByRole("menuitem", { name: "Copy link" }));
	await waitFor(() => expect(write).toHaveBeenLastCalledWith(uri));
	expect(mocks.resolve.mock.calls.length).toBeGreaterThanOrEqual(3);
	cache.clear();
});
