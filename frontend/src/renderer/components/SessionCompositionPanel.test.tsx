import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { compositionFixture } from "../test/fixtures/rca-composition";
import { SessionCompositionPanel } from "./SessionCompositionPanel";

const client = vi.hoisted(() => ({ get: vi.fn(), set: vi.fn(), clear: vi.fn(), purge: vi.fn(), emergencyStop: vi.fn() }));
vi.mock("../lib/api-client", () => ({ compositionClient: client }));
beforeEach(() => {
	vi.clearAllMocks();
	client.get.mockResolvedValue(compositionFixture());
	for (const method of [client.set, client.clear, client.purge, client.emergencyStop]) method.mockResolvedValue({});
});

it("separates recorded estimates, canonical usage and baseline without inventing missing values", async () => {
	render(<SessionCompositionPanel sessionId="worker-1" />);
	const table = await screen.findByRole("table", { name: "Recorded content estimates" });
	expect(within(table).getByRole("row", { name: "Tool results 12 3" })).toBeInTheDocument();
	expect(within(table).getByRole("row", { name: "Visible retained thinking UNKNOWN UNKNOWN" })).toBeInTheDocument();
	expect(screen.getByText("Hidden thinking is not observed")).toBeInTheDocument();
	expect(screen.getByText(/Complete request composition: UNKNOWN/)).toBeInTheDocument();
	expect(screen.getByRole("table", { name: "Canonical usage" })).toHaveTextContent("42");
	expect(screen.getByText("Baseline, not this request")).toBeInTheDocument();
	expect(screen.getByText("UNKNOWN bytes · UNKNOWN estimated tokens")).toBeInTheDocument();
	expect(client.get).toHaveBeenCalledWith({ sessionId: "worker-1" });
});

it("changes recorded granularity without joining canonical events to turns", async () => {
	const data = compositionFixture();
	const snapshot = data.recorded?.snapshots[0];
	if (!snapshot) throw new Error("missing fixture");
	snapshot.turns[0].buckets = [{ category: "tool_result", bytes: 8, estimatedTokens: 2 }];
	client.get.mockResolvedValue(data);
	render(<SessionCompositionPanel sessionId="worker-1" />);
	await screen.findByRole("table", { name: "Recorded content estimates" });
	await userEvent.click(screen.getByRole("combobox", { name: "Snapshot granularity" }));
	await userEvent.click(screen.getByRole("option", { name: "Recorded turn 1" }));
	expect(screen.getByRole("row", { name: "Tool results 8 2" })).toBeInTheDocument();
	expect(screen.getByRole("table", { name: "Canonical usage" })).toHaveTextContent("42");
});

it("uses real hook controls and immediately withdraws a disabled projection", async () => {
	const data = compositionFixture();
	render(<SessionCompositionPanel sessionId="worker-1" />);
	await screen.findByRole("table", { name: "Recorded content estimates" });
	client.get.mockResolvedValue({ ...data, policy: { ...data.policy, effectiveEnabled: false }, status: "unavailable", reason: "disabled" });
	await userEvent.click(screen.getByRole("button", { name: "Stop RCA for session" }));
	await waitFor(() => expect(screen.queryByRole("table", { name: "Recorded content estimates" })).not.toBeInTheDocument());
	expect(client.set).toHaveBeenCalledWith({ sessionId: "worker-1", enabled: false });
	expect(screen.getByRole("table", { name: "Canonical usage" })).toHaveTextContent("42");
	await userEvent.click(screen.getByRole("button", { name: "Delete estimates" }));
	await waitFor(() => expect(client.purge).toHaveBeenCalledWith({ sessionId: "worker-1" }));
	await userEvent.click(screen.getByRole("button", { name: "Pause RCA on host" }));
	await waitFor(() => expect(client.emergencyStop).toHaveBeenCalledWith({ enabled: true }));
});

it("never renders unknown quality strings or transport canaries", async () => {
	const data = compositionFixture();
	data.reason = "A_FAKE_CREDENTIAL";
	if (data.recorded) data.recorded.snapshots[0].reasons.push("B_PRIVATE_PATH");
	client.get.mockResolvedValue(data);
	const { container } = render(<SessionCompositionPanel sessionId="worker-1" />);
	await screen.findByRole("table", { name: "Recorded content estimates" });
	expect(container.textContent).not.toMatch(/A_FAKE|B_PRIVATE/);
	client.get.mockRejectedValue(new Error("A_FAKE_CREDENTIAL B_PRIVATE_PATH"));
	await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
	expect(await screen.findByRole("alert")).toHaveTextContent("Recorded content unavailable");
	expect(container.textContent).not.toMatch(/A_FAKE|B_PRIVATE/);
});

it("preserves fixed API quality codes without echoing arbitrary strings", async () => {
	const data = compositionFixture();
	if (!data.recorded) throw new Error("missing fixture");
	data.recorded.snapshots[0].reasons = ["invalid_record"];
	client.get.mockResolvedValue(data);
	render(<SessionCompositionPanel sessionId="worker-1" />);
	expect(await screen.findByText("invalid_record", { exact: true })).toBeInTheDocument();
	client.get.mockResolvedValue({ ...data, status: "unavailable", reason: "storage_unavailable" });
	await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
	expect(await screen.findByText("Unavailable · storage_unavailable", { exact: true })).toBeInTheDocument();
});

it("shows Claude observed attempts independently from canonical usage and recorded offsets", async () => {
	const data = compositionFixture();
	const buckets = data.recorded?.snapshots[0].buckets;
	if (!buckets) throw new Error("missing numeric fixture");
	data.recorded = null;
	data.claudeInline = {
		projectId: data.projectId,
		sessionId: data.sessionId,
		captureId: "ao-capture",
		role: "orchestrator",
		requested: true,
		active: true,
		reason: "",
		coverage: "observed_inline_attempt",
		method: "utf8_bytes_div4_v1",
		observedAt: "2026-11-10T00:00:00Z",
		unpaired: 1,
		droppedBatches: 2,
		attempts: [
			{ ordinal: 1, buckets, mcpBytes: null, reasons: ["unmatched_attempt", "hidden_thinking"], canonicalEventId: null },
			{
				ordinal: 2,
				buckets: [{ category: "conversation_history", bytes: 20, estimatedTokens: 5 }],
				mcpBytes: null,
				reasons: [],
				canonicalEventId: null,
			},
		],
	};
	client.get.mockResolvedValue(data);
	render(<SessionCompositionPanel sessionId="worker-1" />);
	expect(await screen.findByRole("table", { name: "Observed inline content estimates" })).toHaveTextContent("Tool results");
	expect(screen.getByText("Requested: On · Source: Active")).toBeInTheDocument();
	expect(screen.getByText(/Orchestrator: Inherit/)).toBeInTheDocument();
	expect(screen.getByText("Hidden thinking is not observed")).toBeInTheDocument();
	expect(screen.queryByRole("button", { name: "Analyze selected session" })).not.toBeInTheDocument();
	expect(screen.queryByRole("combobox", { name: "Snapshot granularity" })).not.toBeInTheDocument();
	await userEvent.click(screen.getByRole("combobox", { name: "Observed attempt" }));
	await userEvent.click(screen.getByRole("option", { name: "Observed attempt 2" }));
	expect(screen.getByRole("row", { name: "Conversation / history 20 5" })).toBeInTheDocument();
	expect(screen.getByRole("table", { name: "Canonical usage" })).toHaveTextContent("42");
	expect(screen.getByText(/Complete request composition: UNKNOWN/)).toBeInTheDocument();
});

it("keeps a requested Claude source off while waiting for an approved launch", async () => {
	const data = compositionFixture();
	data.recorded = null;
	data.status = "unavailable";
	data.reason = "waiting_safe_launch";
	data.claudeInline = {
		projectId: data.projectId,
		sessionId: data.sessionId,
		captureId: "",
		role: "worker",
		requested: true,
		active: false,
		reason: "waiting_safe_launch",
		coverage: "observed_inline_attempt",
		method: "utf8_bytes_div4_v1",
		observedAt: "",
		unpaired: 0,
		droppedBatches: 0,
		attempts: [],
	};
	client.get.mockResolvedValue(data);
	render(<SessionCompositionPanel sessionId="worker-1" />);
	expect(await screen.findByText("Requested: On · Source: Off")).toBeInTheDocument();
	expect(screen.getAllByText(/waiting for a normal approved launch/).length).toBeGreaterThan(0);
	expect(screen.queryByRole("table", { name: "Observed inline content estimates" })).not.toBeInTheDocument();
	expect(client.set).not.toHaveBeenCalled();
	await userEvent.click(screen.getByRole("button", { name: "Stop RCA for session" }));
	await waitFor(() => expect(client.set).toHaveBeenCalledWith({ sessionId: "worker-1", enabled: false }));
});
