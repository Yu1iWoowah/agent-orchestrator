import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nextProvider } from "react-i18next";
import { createFullAppI18n } from "../../src/renderer/i18n/all-messages";
import { aoBridge } from "../../src/renderer/lib/bridge";
import { AppDiagnosticsSettings } from "../../src/renderer/components/settings/AppDiagnosticsSettings";
import type { DiagnosticConsent, DiagnosticsStatus, EnableRequest } from "../../src/shared/app-diagnostics";
import "../../src/renderer/styles.css";
const params = new URLSearchParams(location.search);
const i18n = createFullAppI18n(params.get("lang") === "zh-TW" ? "zh-TW" : "en");
document.documentElement.classList.toggle("dark", params.get("theme") !== "light");
const queryClient = new QueryClient({ defaultOptions: { queries: { enabled: false, retry: false } } });
const sessionId = "next.js-orchestrator-with-a-long-session-identifier-for-diagnostics-1";
queryClient.setQueryData(["workspaces"], [{ id: "next.js", path: "", sessions: [{ id: sessionId, kind: "orchestrator" }] }]);
let consent: DiagnosticConsent | null =
	params.get("state") === "active" ? { grantId: "grant-1", sessionId, expiresAt: Date.now() + 900000 } : null;
const listeners = new Set<(value: DiagnosticsStatus) => void>();
const status = (): DiagnosticsStatus =>
	params.get("state") === "adopted"
		? { state: "unavailable", reason: "adopted_daemon" }
		: consent
			? { state: "ready", reason: "ready" }
			: { state: "disabled", reason: "consent_required" };
const publish = () => {
	for (const listener of listeners) listener(status());
};
Object.assign(aoBridge.appDiagnostics, {
	getStatus: async () => status(),
	getConsent: async () => consent,
	onStatus: (fn: (value: DiagnosticsStatus) => void) => {
		listeners.add(fn);
		return () => listeners.delete(fn);
	},
	getIdentity: async () => ({
		documentId: "fixture",
		windowId: "window-fixture",
		generation: 1,
		enabled: !!consent,
		expiresAt: consent?.expiresAt ?? 0,
	}),
	requestEnable: async (request: EnableRequest) => {
		consent = { grantId: "grant-1", sessionId, expiresAt: Date.now() + 900000 };
		publish();
		return { ...request, outcome: "enabled", ...consent };
	},
	revoke: async () => {
		consent = null;
		publish();
		if (params.get("error") === "cleanup") throw new Error("APP_DIAGNOSTICS_STORAGE_FAILED");
	},
	audit: async () => ({
		records: [{ at: new Date().toISOString(), outcome: "completed", bytes: 512, partial: true }],
		partial: false,
		droppedCount: 0,
	}),
});
createRoot(document.getElementById("root")!).render(
	<I18nextProvider i18n={i18n}>
		<QueryClientProvider client={queryClient}>
			<main className="min-h-screen bg-settings-bg p-4 text-settings-text">
				<p className="mb-4 text-sm text-settings-muted">Renderer fixture — native consent not exercised</p>
				<AppDiagnosticsSettings />
			</main>
		</QueryClientProvider>
	</I18nextProvider>,
);
