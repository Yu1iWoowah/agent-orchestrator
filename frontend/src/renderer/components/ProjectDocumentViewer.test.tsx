import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useUiStore } from "../stores/ui-store";
import { ProjectDocumentViewer } from "./ProjectDocumentViewer";

const mocks = vi.hoisted(() => ({ get: vi.fn(), navigate: vi.fn() }));
vi.mock("../lib/api-client", () => ({
	apiClient: { GET: mocks.get },
	apiErrorMessage: (error: { message: string }) => error.message,
	getApiBaseUrl: () => "http://127.0.0.1:9999",
}));
vi.mock("../lib/use-session-link-navigation", () => ({ useSessionLinkNavigation: () => mocks.navigate }));
function show() {
	useUiStore.setState({ projectDocumentTarget: { kind: "shared", projectId: "project", path: "handoff/doc.md" } });
	return render(
		<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
			<ProjectDocumentViewer />
		</QueryClientProvider>,
	);
}
afterEach(() => {
	mocks.get.mockReset();
	mocks.navigate.mockReset();
	useUiStore.setState({ projectDocumentTarget: null });
});
describe("shared document viewer", () => {
	it("renders Markdown with confined relative assets and no editor", async () => {
		mocks.get.mockResolvedValue({
			data: {
				file: {
					path: "handoff/doc.md",
					content:
						"# Handoff\n\n[Rules](../rules/main.md)\n\n![Diagram](../architecture/a%20b.png)\n\n<script>bad()</script>",
					hash: "sha256-doc",
					size: 100,
					binary: false,
				},
			},
		});
		show();
		expect(await screen.findByRole("heading", { name: "Handoff" })).toBeInTheDocument();
		expect(screen.getByText(/sha256-doc/)).toBeInTheDocument();
		expect(screen.getByRole("img", { name: "Diagram" })).toHaveAttribute(
			"src",
			"http://127.0.0.1:9999/api/v1/projects/project/documents/files/architecture/a%20b.png",
		);
		fireEvent.click(screen.getByRole("link", { name: "Rules" }));
		expect(mocks.navigate).toHaveBeenCalledWith("ao://projects/project/shared/rules/main.md");
		expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
		expect(document.querySelector("script")).toBeNull();
		expect(screen.getByRole("button", { name: "Download document" })).toBeEnabled();
		expect(screen.getByRole("img", { name: "Diagram" })).toHaveAttribute("crossorigin", "anonymous");
		fireEvent.click(screen.getByRole("button", { name: "Source" }));
		expect(screen.getByText(/# Handoff/)).toBeInTheDocument();
	});
	it("keeps named errors visible without substituting session artifacts", async () => {
		mocks.get.mockResolvedValue({ error: { code: "PROJECT_DOCUMENT_NOT_FOUND", message: "missing" } });
		show();
		expect(await screen.findByRole("alert")).toHaveTextContent("PROJECT_DOCUMENT_NOT_FOUND: missing");
		expect(mocks.get).toHaveBeenCalledTimes(1);
		expect(mocks.get).toHaveBeenCalledWith(
			"/api/v1/projects/{id}/documents/file",
			expect.objectContaining({ params: { path: { id: "project" }, query: { path: "handoff/doc.md" } } }),
		);
	});
});
