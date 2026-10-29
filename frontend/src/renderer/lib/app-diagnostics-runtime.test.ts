import { QueryClient } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AoBridge } from "../../preload";
import type { CollectorRequest, DiagnosticDocument } from "../../shared/app-diagnostics";
import { startAppDiagnosticsRenderer } from "./app-diagnostics-runtime";
import { diagnosticsEnabled, setDiagnosticRecording } from "./app-diagnostics-registry";
const stops: (() => void)[] = [];
afterEach(() => {
	for (const stop of stops.splice(0)) stop();
	setDiagnosticRecording(false);
	vi.useRealTimers();
});
function fixture() {
	let configure: (value: DiagnosticDocument) => void = () => undefined;
	let request: (value: CollectorRequest) => void = () => undefined;
	let boot: (value: DiagnosticDocument) => void = () => undefined;
	const submit = vi.fn(async () => true);
	const unsubscribe = vi.fn();
	const observeRoute = vi.fn(() => vi.fn());
	const bridge: AoBridge["appDiagnostics"] = {
 getConsent: async () => null,
 requestEnable: async () => { throw new Error("APP_DIAGNOSTICS_UNSUPPORTED"); },
 revoke: async () => { throw new Error("APP_DIAGNOSTICS_UNSUPPORTED"); },
 audit: async () => { throw new Error("APP_DIAGNOSTICS_UNSUPPORTED"); },
		getStatus: async () => ({ state: "disabled", reason: "consent_required" }),
		register: () =>
			new Promise((resolve) => {
				boot = resolve;
			}),
		submit,
		onConfigure: (fn) => {
			configure = fn;
			return unsubscribe;
		},
		onRequest: (fn) => {
			request = fn;
			return unsubscribe;
		},
		onCancel: () => unsubscribe,
		onStatus: () => unsubscribe,
	};
	const queryClient = new QueryClient();
	queryClient.setQueryData(["workspaces"], []);
	const stop = startAppDiagnosticsRenderer({
		bridge,
		queryClient,
		observeRoute,
		readRoute: () => ({ routeId: "home", source: "local", hostId: "", projectId: "", sessionId: "", revision: 1 }),
	});
	stops.push(stop);
	const document: DiagnosticDocument = {
		documentId: "document-1",
		windowId: "window-1",
		generation: 1,
		enabled: true,
		expiresAt: Date.now() + 900000,
	};
	const incoming: CollectorRequest = {
		schemaVersion: 1,
		requestId: "request-1",
		documentId: "document-1",
		windowId: "window-1",
		generation: 1,
		projectId: "",
		deadline: Date.now() + 5000,
	};
	return {
		configure: (d = document) => configure(d),
		request: (r = incoming) => request(r),
		boot: (d: DiagnosticDocument) => boot(d),
		document,
		incoming,
		submit,
		unsubscribe,
		observeRoute,
		stop,
	};
}
describe("renderer diagnostic activation lifecycle", () => {
	it("stays disabled without native consent and leaves no recording hooks", async () => {
		const h = fixture();
		h.boot({ ...h.document, enabled: false, expiresAt: 0 });
		await Promise.resolve();
		h.request();
		expect(diagnosticsEnabled()).toBe(false);
		expect(h.observeRoute).not.toHaveBeenCalled();
		expect(h.submit).not.toHaveBeenCalled();
	});
	it("does not let a stale bootstrap overwrite newer consent configuration", async () => {
		const h = fixture();
		h.configure();
		h.boot({ ...h.document, enabled: false, expiresAt: 0 });
		await Promise.resolve();
		h.request();
		expect(h.submit).toHaveBeenCalledTimes(1);
	});
	it("rejects old documents, generations, cancelled/replayed requests and clears on revoke", async () => {
		const h = fixture();
		h.boot(h.document);
		await Promise.resolve();
		h.request({ ...h.incoming, documentId: "old" });
		h.request({ ...h.incoming, generation: 2 });
		expect(h.submit).not.toHaveBeenCalled();
		h.request();
		h.request();
		expect(h.submit).toHaveBeenCalledTimes(1);
		h.configure({ ...h.document, enabled: false });
		h.request({ ...h.incoming, requestId: "request-2" });
		expect(h.submit).toHaveBeenCalledTimes(1);
		expect(diagnosticsEnabled()).toBe(false);
		h.stop();
		expect(h.unsubscribe).toHaveBeenCalledTimes(3);
	});
});
