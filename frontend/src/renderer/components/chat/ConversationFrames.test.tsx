import { fireEvent, render as renderView, screen, cleanup, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { TooltipProvider } from "../ui/tooltip";
import { ConversationFrames } from "./ConversationFrames";
import { useConversationFrameStore } from "../../stores/conversation-frame-store";

const render = (view: React.ReactNode) => renderView(view, { wrapper: TooltipProvider });

describe("conversation frames", () => {
	beforeEach(() => useConversationFrameStore.setState({ sessions: {} }));
	afterEach(cleanup);
	it("starts collapsed, stays explicitly open through updates and restores after remount", async () => {
		const surface = document.createElement("section"); document.body.append(surface);
		const props = { sessionId: "frame-test", surface, safeRect: { left: 16, top: 48, width: 700, height: 600 }, frames: [{ kind: "plan" as const, title: "Plan", summary: "1/2", content: <p>First step</p> }] };
		const view = render(<ConversationFrames {...props} />);
		expect(screen.queryByText("First step")).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "Plan 1/2", expanded: false }));
		expect(screen.getByText("First step")).toBeVisible();
		view.rerender(<ConversationFrames {...props} frames={[{ ...props.frames[0], summary: "2/2", content: <p>Completed step</p> }]} />);
		expect(screen.getByText("Completed step")).toBeVisible();
		view.unmount(); render(<ConversationFrames {...props} />);
		expect(screen.getByText("First step")).toBeVisible();
		fireEvent.keyDown(screen.getByRole("region", { name: "Plan" }), { key: "Escape" });
		expect(screen.queryByText("First step")).not.toBeInTheDocument();
		await waitFor(() => expect(screen.getByRole("button", { name: "Plan 1/2", expanded: false })).toHaveFocus());
		surface.remove();
	});
	it("temporarily compacts without overwriting userOpen and restores when space returns", () => {
		const surface = document.createElement("section"); document.body.append(surface);
		const props = { sessionId: "compact-test", surface, safeRect: { left: 16, top: 48, width: 700, height: 600 }, frames: [{ kind: "plan" as const, title: "Plan", summary: "", content: <p>Plan content</p> }] };
		const view = render(<ConversationFrames {...props} />);
		fireEvent.click(screen.getByRole("button", { name: "Plan", expanded: false }));
		view.rerender(<ConversationFrames {...props} safeRect={{ ...props.safeRect, height: 199 }} />);
		expect(screen.queryByText("Plan content")).not.toBeInTheDocument();
		view.rerender(<ConversationFrames {...props} />);
		expect(screen.getByText("Plan content")).toBeVisible();
		surface.remove();
	});
});
