import { useTranslation } from "react-i18next";
import { useSessionAgentSettings } from "../hooks/useSessionAgentSettings";
import { verifiedSettingsActor, type AgentSettingsSelection } from "../lib/agent-settings";

function selectionText(selection: AgentSettingsSelection, fallback: string) {
	return `${selection.model || fallback} · ${selection.effort || fallback} · fast ${selection.fastMode}`;
}

/** All placements share the scoped cache. No requested value is an effective receipt. */
export function AgentSettingsStatus({ sessionId, hostId, enabled, placement }: {
	sessionId: string; hostId?: string; enabled: boolean; placement: "header" | "composer" | "timeline";
}) {
	const { t } = useTranslation();
	const query = useSessionAgentSettings({ sessionId, hostId, enabled });
	if (!enabled || !query.data) return null;
	const snapshot = query.data;
	const fallback = t("agentSettings.providerDefault");
	if (placement === "timeline") {
		const history = snapshot.history?.filter((entry) => verifiedSettingsActor(entry));
		if (!history?.length) return null;
		return <ol aria-label={t("agentSettings.history")} className="space-y-2 text-xs text-muted-foreground" data-testid="agent-settings-activities">
			{[...history].reverse().map((entry) => <li className="rounded-md border border-border p-2 [overflow-wrap:anywhere]" key={entry.id}>
				<p>{t("agentSettings.byOrchestrator", { id: verifiedSettingsActor(entry) })} · <time dateTime={entry.updatedAt}>{entry.updatedAt}</time></p>
				<p>{t("agentSettings.preference")}: {selectionText(entry.source, fallback)} → {selectionText(entry.target, fallback)}</p>
				<p>{t("agentSettings.result")} · {entry.phase}{entry.errorCode ? ` · ${entry.errorCode}` : ""}</p>
				<p>{t("agentSettings.effective")}: {t("agentSettings.unknown")}</p>
			</li>)}
		</ol>;
	}
	if (placement === "composer" && !snapshot.pending) return null;
	return <div aria-label={t(placement === "header" ? "agentSettings.summary" : "agentSettings.pending")} className="min-w-0 shrink-0 space-y-1 px-3 py-1.5 text-xs text-muted-foreground [overflow-wrap:anywhere]" data-testid={`agent-settings-${placement}`}>
		{placement === "header" && <>
			<p className="truncate" title={selectionText(snapshot.settings, fallback)}>{t("agentSettings.current")}: {selectionText(snapshot.settings, fallback)}</p>
			<p>{t("agentSettings.fastEffective")}: {snapshot.effective.fastMode.state === "known" ? snapshot.effective.fastMode.value || fallback : t(snapshot.effective.fastMode.state === "not_provided" ? "agentSettings.notProvided" : "agentSettings.unknown")}</p>
		</>}
		{snapshot.pending && <p role="status">{t(snapshot.pending.applyMode === "restart" ? "agentSettings.pendingRestart" : "agentSettings.pending")}: {selectionText(snapshot.pending.target, fallback)}</p>}
	</div>;
}
