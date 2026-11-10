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
