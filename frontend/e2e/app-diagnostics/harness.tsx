import { createRoot } from "react-dom/client";
import { I18nextProvider } from "react-i18next";
import { createFullAppI18n } from "../../src/renderer/i18n/all-messages";
import { aoBridge } from "../../src/renderer/lib/bridge";
import { AppDiagnosticsSettings } from "../../src/renderer/components/settings/AppDiagnosticsSettings";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ReportProblemContent } from "../../src/renderer/components/settings/ReportProblemContent";
import { TooltipProvider } from "../../src/renderer/components/ui/tooltip";
import "../../src/renderer/styles.css";

// Isolated renderer fixture: no daemon, native consent or product restart.
const params = new URLSearchParams(location.search);
const adopted = params.get("state") !== "disabled";
aoBridge.appDiagnostics.getStatus = async () =>
	adopted ? { state: "unavailable", reason: "adopted_daemon" } : { state: "disabled", reason: "consent_required" };
const queryClient = new QueryClient({ defaultOptions: { queries: { enabled: false, retry: false } } });
queryClient.setQueryData(["workspaces"], []);
const i18n = createFullAppI18n(params.get("lang") === "en" ? "en" : "zh-TW");
createRoot(document.getElementById("root")!).render(
	<I18nextProvider i18n={i18n}>
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>
				<main className="min-h-screen bg-settings-bg p-6 text-settings-text">
					<section className="mx-auto max-w-xl">
						<AppDiagnosticsSettings />
						<ReportProblemContent active />
					</section>
				</main>
			</TooltipProvider>
		</QueryClientProvider>
	</I18nextProvider>,
);
