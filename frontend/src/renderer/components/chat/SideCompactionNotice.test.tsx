import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import type { components } from "../../../api/schema";
import { SideCompactionNotice } from "./SideCompactionNotice";
import { ProtectedSides } from "../ProtectedSides";
import { visibleGlobalSettings } from "../settings/settingsCatalog";
import { useUiStore } from "../../stores/ui-store";

const { get, remove } = vi.hoisted(() => ({ get: vi.fn(), remove: vi.fn() }));
vi.mock("../../lib/api-client", async (original) => ({
	...await original<typeof import("../../lib/api-client")>(), apiClient: { GET: get, DELETE: remove },
}));
const protection: components["schemas"]["SideProtection"] = {
	result: "unknown", historical: true, closed: false, isolated: true, shutdownBlocked: true, reason: "codex_full_access",
};
beforeEach(() => { get.mockReset(); remove.mockReset(); });

it("shows a blocked, isolated unknown and the explicit close escape", async () => {
	const close = vi.fn();
	render(<SideCompactionNotice protection={protection} onClose={close} />);
	expect(screen.getByText("Compaction result unknown (preserved)")).toBeVisible();
	expect(screen.getByText("Isolated: sending and resuming are blocked.")).toBeVisible();
	expect(screen.getByText("Currently blocks shutdown and updates.")).toBeVisible();
	expect(screen.getByText(/Codex full-access requires explicit close/)).toBeVisible();
	await userEvent.click(screen.getByRole("button", { name: "Close side" }));
	expect(close).toHaveBeenCalledOnce();
	await userEvent.click(screen.getByText("Residual risks"));
	expect(screen.getByText(/outside AO/)).toBeVisible();
	expect(screen.getByText(/Codex MCP servers run outside the tool sandbox/)).toBeVisible();
	expect(screen.getByText(/CODEX_HOME/)).toBeVisible();
});

it("keeps unknown and isolation after veto release", () => {
	render(<SideCompactionNotice protection={{ ...protection, shutdownBlocked: false, reason: "scope_host_gone_group_esrch" }} onClose={vi.fn()} />);
	expect(screen.getByText(/Does not currently block shutdown/)).toBeVisible();
	expect(screen.getByText("Compaction result unknown (preserved)")).toBeVisible();
	expect(screen.getByRole("button", { name: "Close side" })).toBeVisible();
});
it("shows closure without another close action", () => {
	render(<SideCompactionNotice protection={{ ...protection, closed: true, isolated: false, shutdownBlocked: false }} onClose={vi.fn()} />);
	expect(screen.getByText("Side closed. It no longer blocks shutdown.")).toBeVisible();
	expect(screen.queryByRole("button", { name: "Close side" })).not.toBeInTheDocument();
});
it("does not change ordinary side UI", () => {
	const { container } = render(<SideCompactionNotice onClose={vi.fn()} />);
	expect(container).toBeEmptyDOMElement();
});
it("does not present a pending result as historical unknown", () => {
	render(<SideCompactionNotice protection={{ ...protection, result: "pending", historical: false }} onClose={vi.fn()} />);
	expect(screen.getByText("Compaction result not yet known (preserved)")).toBeVisible();
});

it.each(["deleted", "TUI", "ended", "new-launch-parent-present"])("finds and closes protected side with %s parent, even with side features off", async (parent) => {
	useUiStore.setState({ developerMode: false, sideChats: false });
	const side = { sideId: "old-side", sessionId: "old-parent", provider: "codex", state: "unknown", historical: true, orphan: parent !== "new-launch-parent-present", protection };
	get.mockResolvedValue({ data: { sides: [side] } });
	remove.mockImplementation(async () => { get.mockResolvedValue({ data: { sides: [] } }); return {}; });
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const view = render(<QueryClientProvider client={client}><ProtectedSides /></QueryClientProvider>);
	expect(await screen.findByText("old-side · codex")).toBeVisible();
	expect(Boolean(screen.queryByText(/Parent ended, deleted, or switched to TUI/))).toBe(side.orphan);
	await userEvent.click(screen.getByRole("button", { name: "Close side" }));
	expect(remove).not.toHaveBeenCalled();
	await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close side" }));
	await screen.findByText("No unresolved protected sides.");
	expect(remove).toHaveBeenCalledWith("/api/v1/side-chats/protected/{sideId}", { params: { path: { sideId: "old-side" } } });
	view.unmount(); client.clear();
});

it("lists protected sides in settings without developer mode", () => {
	expect(visibleGlobalSettings({ cloudEnabled: false, developerMode: false, diagnostics: false, customCoder: false, selfServeBilling: false }).some((section) => section.id === "protectedSides")).toBe(true);
});

it("retains the entry and error when close fails", async () => {
	get.mockResolvedValue({ data: { sides: [{ sideId: "old-side", sessionId: "parent", provider: "codex", orphan: true, protection }] } });
	remove.mockResolvedValue({ error: { message: "Close failed" } });
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const view = render(<QueryClientProvider client={client}><ProtectedSides /></QueryClientProvider>);
	await userEvent.click(await screen.findByRole("button", { name: "Close side" }));
	await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close side" }));
	await waitFor(() => expect(screen.getByText("Close failed")).toBeVisible());
	expect(screen.getByText("old-side · codex")).toBeVisible();
	view.unmount(); client.clear();
});
