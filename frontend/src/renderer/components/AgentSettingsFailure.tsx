import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { agentSettingsQueryKey, latestAgentSettingsFailure, rebaseAgentSettingsSelection, type AgentSettingsOperation, type AgentSettingsSnapshot } from "../lib/agent-settings";
import { AgentSettingsEditor } from "./AgentSettingsEditor";

const reasonKeys = {
	provider: "agentSettings.failureProvider",
	conflict: "agentSettings.failureConflict",
	storage: "agentSettings.failureStorage",
	validation: "agentSettings.failureValidation",
	cancelled: "agentSettings.failureCancelled",
	recovery_required: "agentSettings.failureRecovery",
} as const satisfies Record<NonNullable<AgentSettingsOperation["errorCategory"]>, string>;

/** Dismissal is local presentation state, never a journal mutation. Keep the
 * retry editor mounted across external results so CDC cannot erase its draft. */
export function AgentSettingsFailure({ snapshot, sessionId, hostId, terminated = false }: {
	snapshot: AgentSettingsSnapshot; sessionId: string; hostId?: string; terminated?: boolean;
}) {
	const { t } = useTranslation();
	const client = useQueryClient();
	const dismissalKey = ["agent-settings-failure-dismissal", ...agentSettingsQueryKey(sessionId, hostId)];
	const dismissed = useQuery<string | null>({ queryKey: dismissalKey, queryFn: () => null, initialData: null, enabled: false });
	const [retry, setRetry] = useState<AgentSettingsOperation>();
	const failure = latestAgentSettingsFailure(snapshot);
	const canRetry = !terminated && !snapshot.writerRecovery && failure?.errorCategory !== "recovery_required";
	const fallback = t("agentSettings.providerDefault");
	return <>
		{failure && dismissed.data !== failure.id && <div role="alert" className="space-y-1 rounded-md border border-destructive/30 p-2 text-xs text-destructive [overflow-wrap:anywhere]">
			<p className="font-medium">{t("agentSettings.failureTitle")}</p>
			<p>{t(failure.errorCategory ? reasonKeys[failure.errorCategory] : "agentSettings.failureUnknown")}</p>
			<p>{t("agentSettings.preference")}: {failure.target.model || fallback} · {failure.target.effort || fallback} · {t("agentSettings.fast")} {failure.target.fastMode}</p>
			<p className="font-mono">{failure.errorCode || "AGENT_SETTINGS_FAILED"} · {failure.id}{failure.requestId ? ` · ${failure.requestId}` : ""}</p>
			<p className="text-muted-foreground">{t(canRetry ? "agentSettings.failureNext" : "agentSettings.failureKeepOnly")}</p>
			<div className="flex flex-wrap gap-x-3 gap-y-1">
				{canRetry && <button className="underline" onClick={() => setRetry(failure)} type="button">{t("agentSettings.reviewRetry")}</button>}
				<button className="underline" onClick={() => client.setQueryData(dismissalKey, failure.id)} type="button">{t("agentSettings.keepCurrent")}</button>
			</div>
		</div>}
		{retry && <AgentSettingsEditor hostId={hostId} key={retry.id} retry={{ selection: rebaseAgentSettingsSelection(retry.source, retry.target, snapshot.settings), onClose: () => setRetry(undefined) }} sessionId={sessionId} snapshot={snapshot} />}
	</>;
}
