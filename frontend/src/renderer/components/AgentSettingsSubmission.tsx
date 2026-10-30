import { useTranslation } from "react-i18next";
import type { TurnSettings } from "../types/conversation";

export function AgentSettingsSubmission({ pending, error, draft, onRetry, onDismiss }: {
	pending: boolean; error?: string; draft?: TurnSettings; onRetry: () => void; onDismiss: () => void;
}) {
	const { t } = useTranslation();
	if (pending) return <p className="px-3 py-1 text-xs text-muted-foreground" data-testid="agent-settings-submission" role="status">{t("agentSettings.submitting")}</p>;
	if (!error || !draft) return null;
	return <div className="space-y-1 rounded-md border border-destructive/30 p-2 text-xs text-destructive [overflow-wrap:anywhere]" data-testid="agent-settings-submission" role="alert">
		<p className="font-medium">{t("agentSettings.notAccepted")}</p>
		<p>{error}</p>
		<p>{t("agentSettings.unacceptedChoice")}: {draft.model || t("agentSettings.providerDefault")} · {draft.reasoningEffort || t("agentSettings.providerDefault")} · {t("agentSettings.fast")} {draft.fastMode === undefined ? "default" : draft.fastMode ? "on" : "off"}{draft.approvalMode ? ` · ${draft.approvalMode}` : ""}</p>
		<p className="text-muted-foreground">{t("agentSettings.submissionNext")}</p>
		<div className="flex flex-wrap gap-3">
			<button className="underline" onClick={onRetry} type="button">{t("agentSettings.retrySending")}</button>
			<button className="underline" onClick={onDismiss} type="button">{t("agentSettings.keepCurrent")}</button>
		</div>
	</div>;
}
