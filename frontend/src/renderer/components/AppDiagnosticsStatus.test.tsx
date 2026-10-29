import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { appI18n } from "../i18n";
import { AppDiagnosticsStatus } from "./AppDiagnosticsStatus";
const getStatus = vi.hoisted(() => vi.fn());
vi.mock("../lib/bridge", () => ({ aoBridge: { appDiagnostics: { getStatus, onStatus: () => () => undefined } } }));
afterEach(async () => {
	cleanup();
	await appI18n.changeLanguage("en");
});
describe("Q15-A app diagnostic status", () => {
	it.each([
		["en", "App diagnostics unavailable: using an existing daemon. Giving consent again will not fix this connection."],
		["zh-TW", "診斷不可用，原因：沿用既有 daemon。重新同意無法修復此連線。"],
	])("explains adoption in %s without an enable/restart action", async (locale, message) => {
		await appI18n.changeLanguage(locale);
		getStatus.mockResolvedValue({ state: "unavailable", reason: "adopted_daemon" });
		await act(async () => {
			render(<AppDiagnosticsStatus />);
		});
		expect(screen.getByRole("status")).toHaveTextContent(message);
		expect(screen.queryByRole("button")).toBeNull();
		expect(screen.queryByRole("switch")).toBeNull();
	});
	it("does not present normal disabled diagnostics as a daemon failure", async () => {
		getStatus.mockResolvedValue({ state: "disabled", reason: "consent_required" });
		await act(async () => {
			render(<AppDiagnosticsStatus />);
		});
		expect(screen.queryByRole("status")).toBeNull();
	});
});
