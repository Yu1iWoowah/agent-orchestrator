import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionConversationFrames } from "./SessionConversationFrames";
import { TooltipProvider } from "../ui/tooltip";
import { useConversationFrameStore } from "../../stores/conversation-frame-store";

const { get } = vi.hoisted(() => ({ get: vi.fn() }));
vi.mock("../../lib/host-clients", () => ({
  clientForSessionHost: () => ({ GET: get }),
}));
let surface: HTMLElement;
vi.mock("../../hooks/useConversationFrameSafeRect", () => ({
  useConversationFrameSafeRect: () => ({
    surface,
    rect: { left: 16, top: 48, width: 1000, height: 900 },
  }),
}));
let client: QueryClient;
const locate = vi.fn();
const item = {
  branchId: "root",
  actionable: true,
  read: false,
  activity: {
    kind: "activity",
    id: "source-one",
    requestId: "native-request",
    activityKind: "user_input",
    status: "pending",
    sequence: 9,
    revision: 0,
    summary: "Pick a target",
    detail: { message: "Which environment should receive this change?" },
    createdAt: "2026-10-29T00:00:00Z",
  },
};
const page = {
  conversationId: "conv",
  activeBranchId: "root",
  items: [item],
  counts: { open: 9, unread: 4, history: 2 },
  hasMoreBefore: false,
};
function show() {
  return render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <SessionConversationFrames
          sessionId="attention-session"
          activeBranchId="root"
          surfaceRef={{ current: surface }}
          composerRef={{ current: null }}
          onLocateAttention={locate}
        />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}
beforeEach(() => {
  get.mockReset();
  locate.mockReset();
  useConversationFrameStore.setState({ sessions: {} });
  surface = document.createElement("section");
  document.body.append(surface);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  get.mockImplementation((path: string) =>
    Promise.resolve({
      data: path.includes("/attention")
        ? page
        : { state: "absent", activeBranchId: "root" },
      response: { status: 200 },
    }),
  );
});
afterEach(() => {
  cleanup();
  client.clear();
  surface.remove();
});
describe("session attention frame", () => {
  it("opens attention by default, keeps Plan collapsed, and shows server counts beyond the page", async () => {
    show();
    await screen.findByRole("region", { name: "Awaiting you" });
    await screen.findByText("Which environment should receive this change?");
    expect(
      screen.queryByRole("region", { name: "Plan" }),
    ).not.toBeInTheDocument();
    expect(
      within(screen.getByRole("region", { name: "Awaiting you" })).getByText(
        "9",
      ),
    ).toBeInTheDocument();
  });
  it("locates the original request without drawing another native answer form", async () => {
    show();
    await userEvent.click(
      await screen.findByRole("button", { name: /Go to original request/ }),
    );
    expect(locate).toHaveBeenCalledWith("source-one");
    expect(screen.queryByRole("radio")).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  });
  it("does not reopen a manually collapsed frame when a new request arrives", async () => {
    show();
    await screen.findByText("Which environment should receive this change?");
    await userEvent.click(
      screen.getByRole("button", { name: "Collapse Awaiting you" }),
    );
    get.mockImplementation((path: string) =>
      Promise.resolve({
        data: path.includes("/attention")
          ? { ...page, counts: { ...page.counts, open: 10 } }
          : { state: "absent", activeBranchId: "root" },
        response: { status: 200 },
      }),
    );
    await client.invalidateQueries({
      queryKey: ["session-attention", "attention-session"],
    });
    await screen.findByRole("button", { name: "Awaiting you 10" });
    expect(
      screen.queryByRole("region", { name: "Awaiting you" }),
    ).not.toBeInTheDocument();
  });
  it("shows unknown counts and retry on failed reads rather than cached zero", async () => {
    get.mockImplementation((path: string) =>
      path.includes("/attention")
        ? Promise.reject(new Error("offline"))
        : Promise.resolve({
            data: { state: "absent", activeBranchId: "root" },
            response: { status: 200 },
          }),
    );
    show();
    await screen.findByRole("alert");
    expect(
      screen.getByRole("button", { name: "Retry attention" }),
    ).toBeInTheDocument();
    await userEvent.click(
      screen.getByRole("button", { name: "Collapse Awaiting you" }),
    );
    expect(
      screen.getByRole("button", { name: "Awaiting you ?" }),
    ).toBeInTheDocument();
  });
});
