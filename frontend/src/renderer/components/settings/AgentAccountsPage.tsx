import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "../ui/button";
import { AgentAccountsPanel } from "./AgentAccountsSection";
import { CodexAccountsSection } from "./CodexAccountsSection";
/** Local account management shares the panels used by the existing Agents deep link. */
export function AgentAccountsPage() {
	const { t } = useTranslation();
	const [legacyOpen, setLegacyOpen] = useState(false);
	return (
		<div className="min-w-0 space-y-5" data-testid="agent-accounts-page">
			<AgentAccountsPanel provider="claude" />
			<AgentAccountsPanel provider="codex" />
			<div>
				<Button
					type="button"
					variant="ghost"
					aria-expanded={legacyOpen}
					aria-controls="legacy-codex-logins"
					onClick={() => setLegacyOpen(!legacyOpen)}
				>
					{t("settings.agentAccounts.manageLegacyAccounts")}
				</Button>
				{legacyOpen ? (
					<div id="legacy-codex-logins" className="mt-3 space-y-3">
						<p className="text-sm text-muted-foreground">
							{t("settings.agentAccounts.legacyAccountsDescription")}
						</p>
						<CodexAccountsSection title={t("settings.agentAccounts.legacyAccountsTitle")} />
					</div>
				) : null}
			</div>
		</div>
	);
}
