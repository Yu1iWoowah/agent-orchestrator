import { useUiStore, inspectorIsOpen } from "./stores/ui-store";
import { sessionUiKey } from "./lib/hosts";
import { startAppDiagnosticsRenderer } from "./lib/app-diagnostics-runtime";
import { safeId } from "./lib/app-diagnostics-projectors";
import "./lib/apply-initial-theme";
import { createRoot } from "react-dom/client";
import { QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { I18nextProvider } from "react-i18next";
import "@xterm/xterm/css/xterm.css";
import "./styles.css";
import { preloadMarkdownFonts } from "./hooks/useMarkdownFontsReady";
import { queryClient } from "./lib/query-client";
import { mergeUnreadNotification, unreadNotificationsQueryKey } from "./lib/notifications";
import { playNotificationSound } from "./lib/notification-sound-player";
import { createAppRouter } from "./router";
import { TelemetryBoundary } from "./components/TelemetryBoundary";
import { CloudOnboardingGate } from "./components/CloudOnboardingGate";
import { CloudNotificationRuntime } from "./components/CloudNotificationRuntime";
import { TelemetryIdentityRuntime } from "./components/TelemetryIdentityRuntime";
import { applyAnalyticsOptOut, applyRendererTelemetryPolicy, clearRendererTelemetryQueues, initTelemetry, isDeniedEvent } from "./lib/telemetry";
import { aoBridge } from "./lib/bridge";
import { startDaemonFailureTelemetry } from "./lib/daemon-telemetry";
import { startUpdateTelemetry } from "./lib/update-telemetry";
import { startSessionManagementTelemetry } from "./lib/session-management-telemetry";
import { appI18n } from "./i18n";
import { useLocaleStore } from "./stores/locale-store";
import { useSoundNotificationsStore } from "./stores/sound-notifications-store";
import { useTelemetryPolicyStore } from "./stores/telemetry-policy-store";
import { useAnalyticsOptOutStore } from "./stores/analytics-opt-out-store";

void preloadMarkdownFonts();
const router = createAppRouter(queryClient);
let diagnosticRouteRevision = 1;
const stopAppDiagnostics = startAppDiagnosticsRenderer({
	bridge: aoBridge.appDiagnostics,
	queryClient,
	observeRoute: () =>
		router.subscribe("onResolved", () => {
			diagnosticRouteRevision++;
		}),
	readInspectorOpen: (route) =>
		Boolean(route.sessionId) &&
		inspectorIsOpen(useUiStore.getState().inspectorSessions, sessionUiKey(route.sessionId, route.hostId || undefined)),
	readRoute: () => {
		const matches = router.state.matches;
		const last = matches.at(-1);
		const params = last?.params;
		const routeId = last?.routeId ?? "";
		const projectId = params && "projectId" in params ? safeId(params.projectId) : "";
		const sessionId = params && "sessionId" in params ? safeId(params.sessionId) : "";
		const hostId = params && "hostId" in params ? safeId(params.hostId) : "";
		let canonicalRoute = "unsupported_surface";
		if (routeId.includes("$sessionId")) canonicalRoute = "session";
		else if (routeId.includes("settings")) canonicalRoute = "settings";
		else if (routeId.includes("terminal")) canonicalRoute = "terminal";
		else if (routeId.includes("$projectId")) canonicalRoute = "board";
		else if (routeId === "/_shell/") canonicalRoute = "home";
		return {
			routeId: canonicalRoute,
			projectId,
			sessionId,
			hostId,
			source: hostId ? "remote" : routeId.includes("cloud") ? "cloud" : "local",
			revision: diagnosticRouteRevision,
		};
	},
});
window.addEventListener("pagehide", stopAppDiagnostics, { once: true });
if (import.meta.hot) import.meta.hot.dispose(stopAppDiagnostics);

// Main owns consent and only acknowledges opt-out after every live AO shell
// confirms that its in-memory renderer queues were actually purged.
aoBridge.telemetry.onClearQueues(clearRendererTelemetryQueues);
// Main decides *when* a notification sound plays; the renderer only supplies
// the speakers, since main has no audio output of its own.
aoBridge.notifications.onPlaySound(() => {
	playNotificationSound(() => aoBridge.notifications.reportSoundFailure());
});
aoBridge.telemetry.onPolicy((view) => applyRendererTelemetryPolicy(view.eventsEnabled && view.acknowledged && view.state === "applied"));

// The analytics opt-out lives in main (shared with the daemon and mobile). Apply
// it to this renderer's PostHog client at launch and whenever Settings flips it.
void aoBridge.telemetry.getAnalyticsOptOut().then((optedOut) => {
	if (optedOut) void applyAnalyticsOptOut(true);
}).catch(() => undefined);
aoBridge.telemetry.onAnalyticsOptOut((optedOut) => {
	useAnalyticsOptOutStore.setState({ optedOut, loaded: true });
	void applyAnalyticsOptOut(optedOut);
});

if (import.meta.env.DEV) {
	const w = window as never as Record<string, unknown>;
	w.__qc = queryClient;
	// __testNotif("needs_input") — simulates a real notification:
	//   bell count + dock badge update immediately; dock bounces after 3s
	//   (gives you time to click away from AO so the bounce is visible)
	const testNotifTitles: Record<string, string> = {
		needs_input: "Agent needs your input",
		ready_to_merge: "Ready to merge",
		pr_merged: "PR merged",
		pr_closed_unmerged: "PR closed",
	};
	w.__testNotif = async (
		type: "needs_input" | "ready_to_merge" | "pr_merged" | "pr_closed_unmerged" = "needs_input",
	) => {
		const key = unreadNotificationsQueryKey;
		const id = `test-${Date.now()}`;
		// Freeze the query so window-focus refetch doesn't wipe test data
		queryClient.setQueryDefaults(key, { staleTime: 60_000 });
		await queryClient.cancelQueries({ queryKey: key });
		mergeUnreadNotification(queryClient, {
			id,
			type,
			title: testNotifTitles[type] ?? "Notification",
			body: "Test notification",
			createdAt: new Date().toISOString(),
			sessionId: "",
			projectId: "",
			prUrl: "",
			target: { kind: "session", sessionId: "" },
			status: "unread",
		});
		console.log("[testNotif] bell updated - click away from AO now, bounce fires in 3s");
		setTimeout(() => {
			void window.ao?.notifications.devBounce();
			// Restore normal stale time after bounce
			queryClient.setQueryDefaults(key, { staleTime: 0 });
		}, 3000);
	};
}

let sessionManagementTelemetryRetries = 0;
const bootstrapSessionManagementTelemetry = () => {
	void initTelemetry().then((enabled) => {
		if (enabled) {
			startSessionManagementTelemetry();
			return;
		}
		if (!isDeniedEvent("ao.renderer.session_management_summary") && sessionManagementTelemetryRetries++ < 12) {
			window.setTimeout(bootstrapSessionManagementTelemetry, 5_000);
		}
	});
};
bootstrapSessionManagementTelemetry();
startDaemonFailureTelemetry();
startUpdateTelemetry();

declare module "@tanstack/react-router" {
	interface Register {
		router: typeof router;
	}
}

async function renderApp(): Promise<void> {
	void useTelemetryPolicyStore.getState().load();
	// The persisted locale is cosmetic; do not leave a newly opened native
	// window blank while its IPC read completes. The router's pending screen
	// renders immediately, then i18n updates if the user chose another locale.
	void useLocaleStore.getState().load();
	// The sound-notifications toggle only needs to be right by the time
	// Settings renders, so it loads in the background rather than blocking mount.
	void useSoundNotificationsStore.getState().load();
	// Do not wrap the desktop root in StrictMode. React 19 enables per-component
	// performance tracking for that tree in development, which made common route
	// switches and drag updates spend hundreds of milliseconds recording timings.
	createRoot(document.getElementById("root") as HTMLElement).render(
		<I18nextProvider i18n={appI18n}>
			<TelemetryBoundary>
				<QueryClientProvider client={queryClient}>
					<CloudNotificationRuntime />
					<TelemetryIdentityRuntime />
					<RouterProvider router={router} />
					<CloudOnboardingGate />
				</QueryClientProvider>
			</TelemetryBoundary>
		</I18nextProvider>,
	);
}

void renderApp();
