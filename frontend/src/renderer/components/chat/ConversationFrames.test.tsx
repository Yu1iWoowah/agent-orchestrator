import {
	fireEvent,
	render as renderView,
	screen,
	cleanup,
	act,
	waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { TooltipProvider } from "../ui/tooltip";
import { ConversationFrames } from "./ConversationFrames";
import { useConversationFrameStore } from "../../stores/conversation-frame-store";

const render = (view: React.ReactNode) => renderView(view, { wrapper: TooltipProvider });

describe("conversation frames", () => {
	beforeEach(() => useConversationFrameStore.setState({ sessions: {} }));
	afterEach(cleanup);
	it("starts collapsed, stays explicitly open through updates and restores after remount", async () => {
		const surface = document.createElement("section");
		document.body.append(surface);
		const props = {
			sessionId: "frame-test",
			surface,
			safeRect: { left: 16, top: 48, width: 700, height: 600 },
			frames: [
				{ kind: "plan" as const, title: "Plan", summary: "1/2", content: <p>First step</p> },
			],
		};
		const view = render(<ConversationFrames {...props} />);
		expect(screen.queryByText("First step")).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "Plan 1/2", expanded: false }));
		expect(screen.getByText("First step")).toBeVisible();
		view.rerender(
			<ConversationFrames
				{...props}
				frames={[{ ...props.frames[0], summary: "2/2", content: <p>Completed step</p> }]}
			/>,
		);
		expect(screen.getByText("Completed step")).toBeVisible();
		view.unmount();
		render(<ConversationFrames {...props} />);
		expect(screen.getByText("First step")).toBeVisible();
		fireEvent.keyDown(screen.getByRole("region", { name: "Plan" }), { key: "Escape" });
		expect(screen.queryByText("First step")).not.toBeInTheDocument();
		await waitFor(() =>
			expect(screen.getByRole("button", { name: "Plan 1/2", expanded: false })).toHaveFocus(),
		);
		surface.remove();
	});
	it("temporarily compacts without overwriting userOpen and restores when space returns", () => {
		const surface = document.createElement("section");
		document.body.append(surface);
		const props = {
			sessionId: "compact-test",
			surface,
			safeRect: { left: 16, top: 48, width: 700, height: 600 },
			frames: [{ kind: "plan" as const, title: "Plan", summary: "", content: <p>Plan content</p> }],
		};
		const view = render(<ConversationFrames {...props} />);
		fireEvent.click(screen.getByRole("button", { name: "Plan", expanded: false }));
		view.rerender(<ConversationFrames {...props} safeRect={{ ...props.safeRect, height: 199 }} />);
		expect(screen.queryByText("Plan content")).not.toBeInTheDocument();
		view.rerender(<ConversationFrames {...props} />);
		expect(screen.getByText("Plan content")).toBeVisible();
		surface.remove();
	});
	it("keeps a finite committed width after a painted pointer drag", async () => {
		const surface = document.createElement("section");
		document.body.append(surface);
		render(
			<ConversationFrames
				sessionId="pointer-test"
				surface={surface}
				safeRect={{ left: 16, top: 48, width: 700, height: 600 }}
				frames={[{ kind: "plan", title: "Plan", summary: "", content: <p>Drag content</p> }]}
			/>,
		);
		fireEvent.click(screen.getByRole("button", { name: "Plan", expanded: false }));
		const frame = screen.getByRole("region", { name: "Plan" });
		fireEvent.pointerDown(screen.getByRole("separator", { name: "Resize Plan width" }), {
			clientX: 400,
			pointerId: 7,
		});
		fireEvent.pointerMove(window, { clientX: 384, pointerId: 7 });
		await waitFor(() => expect(frame.style.getPropertyValue("--frame-width")).toBe("376px"));
		fireEvent.pointerUp(window, { clientX: 384, pointerId: 7 });
		expect(frame.style.getPropertyValue("--frame-width")).toBe("376px");
		surface.remove();
	});
	it("keeps a compact focus panel mounted while the window grows", () => {
		const surface = document.createElement("section");
		document.body.append(surface);
		const props = {
			sessionId: "peek-grow",
			surface,
			safeRect: { left: 16, top: 48, width: 700, height: 180 },
			frames: [
				{
					kind: "plan" as const,
					title: "Plan",
					summary: "",
					content: <input aria-label="Preserved frame content" defaultValue="draft" />,
				},
			],
		};
		const view = render(<ConversationFrames {...props} />);
		// A saved open preference hard-fits to an entry in this small safe area.
		act(() =>
			useConversationFrameStore
				.getState()
				.setFrameOpen(JSON.stringify(["local", "peek-grow"]), "plan", true),
		);
		fireEvent.click(screen.getByRole("button", { name: "Open Plan" }));
		const input = screen.getByRole("textbox", { name: "Preserved frame content" });
		fireEvent.change(input, { target: { value: "keep this draft" } });
		view.rerender(<ConversationFrames {...props} safeRect={{ ...props.safeRect, height: 600 }} />);
		expect(screen.getByRole("textbox", { name: "Preserved frame content" })).toBe(input);
		expect(input).toHaveValue("keep this draft");
		surface.remove();
	});
});
