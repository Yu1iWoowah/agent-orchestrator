import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { DiagnosticConsent, DiagnosticAudit } from "../../../shared/app-diagnostics";
import { AppDiagnosticsSettings } from "./AppDiagnosticsSettings";
const bridge = vi.hoisted(() => ({
	getStatus: vi.fn(async () => ({ state: "disabled", reason: "consent_required" })),
	getConsent: vi.fn<() => Promise<DiagnosticConsent | null>>(async () => null),
	register: vi.fn(async () => ({ documentId: "doc-1", windowId: "window-1", generation: 1, enabled: false, expiresAt: 0 })),
	requestEnable: vi.fn(async () => ({ outcome: "cancelled" })),
	revoke: vi.fn(async () => undefined),
	audit: vi.fn<() => Promise<DiagnosticAudit>>(async () => ({ records: [], partial: false, droppedCount: 0 })),
	onStatus: () => () => undefined,
}));
vi.mock("../../lib/bridge", () => ({ aoBridge: { appDiagnostics: bridge } }));
vi.mock("@tanstack/react-query", () => ({
	useQuery: () => ({
		data: [
			{
				id: "next.js",
				sessions: [
					{ id: "next.js-1", kind: "orchestrator" },
					{ id: "next.js-2", kind: "worker" },
					{ id: "next.js-3", kind: "orchestrator", isTerminated: true },
				],
			},
		],
	}),
}));
vi.mock("../../hooks/useWorkspaceQuery", () => ({ workspaceQueryOptions: {} }));
afterEach(() => {
	cleanup();
	vi.clearAllMocks();
});
it("starts disabled and submits only the selected live orchestrator for native consent", async () => {
	await act(async () => {
		render(<AppDiagnosticsSettings />);
	});
	expect(screen.getByRole("combobox")).toBeInTheDocument();
	await act(async () => {
		fireEvent.click(screen.getByRole("button", { name: "Allow for 15 minutes" }));
	});
	expect(bridge.requestEnable).toHaveBeenCalledWith(
		expect.objectContaining({ schemaVersion: 1, documentId: "doc-1", generation: 1, sessionId: "next.js-1" }),
	);
	expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
});
it("shows adoption as unavailable with disabled controls", async () => {
	bridge.getStatus.mockResolvedValueOnce({ state: "unavailable", reason: "adopted_daemon" });
	await act(async () => {
		render(<AppDiagnosticsSettings />);
	});
	expect(screen.getByRole("status")).toHaveTextContent("using an existing daemon");
	expect(screen.getByRole("button", { name: "Allow for 15 minutes" })).toBeDisabled();
});

it("shows consent countdown, reads audit and disables access even if cleanup fails", async () => {
	bridge.getConsent.mockResolvedValueOnce({ grantId: "grant-1", sessionId: "next.js-1", expiresAt: Date.now() + 900000 });
	bridge.audit.mockResolvedValueOnce({
		records: [{ at: new Date().toISOString(), outcome: "completed", bytes: 256, partial: true }],
		partial: true,
		droppedCount: 3,
	});
	await act(async () => {
		render(<AppDiagnosticsSettings />);
	});
	expect(screen.getByText(/next.js-1 ·/)).toBeInTheDocument();
	await act(async () => {
		fireEvent.click(screen.getByRole("button", { name: "View audit" }));
	});
	expect(screen.getByRole("table")).toHaveTextContent("Captured · Partial");
	expect(screen.getByRole("status")).toHaveTextContent("3 older records");
	bridge.revoke.mockRejectedValueOnce(new Error("APP_DIAGNOSTICS_STORAGE_FAILED"));
	await act(async () => {
		fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
	});
	expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
	expect(screen.getByRole("alert")).toHaveTextContent("Access is disabled");
	expect(screen.queryByRole("table")).toBeNull();
});
