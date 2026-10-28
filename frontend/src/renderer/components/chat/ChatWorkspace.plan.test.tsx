import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import { renderChat } from "../../test/render-chat";
import { chatFixtureSettled } from "../../lib/chat-fixture";
import { ChatWorkspace } from "./ChatWorkspace";

vi.mock("./SessionPlanFrames", () => ({
	SessionPlanFrames: () => <button>Session Plan entry</button>,
}));

describe("primary session Plan ownership", () => {
	it("removes automatic Plan lists from the timeline while retaining normal prose", () => {
		const snapshot = {
			...chatFixtureSettled,
			turns: chatFixtureSettled.turns.map((turn) => ({
				...turn,
				plan: { steps: [{ text: "Automatic checklist only", status: "pending" as const }] },
			})),
		};
		renderChat(
			<ChatWorkspace snapshot={snapshot} sessionPlanEnabled onSend={vi.fn()} inlineHeader />,
		);
		expect(screen.queryByRole("region", { name: "Agent plan" })).not.toBeInTheDocument();
		expect(screen.queryByText("Automatic checklist only")).not.toBeInTheDocument();
		expect(screen.getByText("Session Plan entry")).toBeInTheDocument();
		expect(screen.getByRole("log", { name: "Conversation" })).toHaveTextContent(/\S/);
	});
});
