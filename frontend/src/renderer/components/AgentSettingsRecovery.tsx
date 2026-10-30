import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useAgentSettingsAction } from "../hooks/useSessionAgentSettings";
import { agentSettingsErrorMessage, canCancelAgentSettings, type AgentSettingsSnapshot } from "../lib/agent-settings";
import { ConfirmDialog } from "./ConfirmDialog";

/** Neither a retry nor an operator checkbox is proof that a writer stopped. */
export function AgentSettingsRecovery({ snapshot, sessionId, hostId, terminated }: {
	snapshot: AgentSettingsSnapshot; sessionId: string; hostId?: string; terminated: boolean;
}) {
	const { t } = useTranslation();
	const action = useAgentSettingsAction();
	const [confirmation, setConfirmation] = useState<"stop" | { operationId: string; version: number }>();
	const [stopIfAlive, setStopIfAlive] = useState(false);
	const error = action.error ? agentSettingsErrorMessage(action.error, t("agentSettings.conflict")) : undefined;
	const pending = snapshot.pending;
	async function confirm() {
		if (!confirmation) return;
		try {
			await action.mutateAsync(confirmation === "stop" ? { kind: "stop", sessionId, hostId } : { kind: "settle", sessionId, hostId, ...confirmation, stopIfAlive });
			setConfirmation(undefined);
		} catch { /* Keep the fence visible and allow a deliberate retry. */ }
	}
	return <div className="space-y-2">
		{pending && canCancelAgentSettings(pending) && <button className="underline" disabled={action.isPending} onClick={() => action.mutate({ kind: "cancel", sessionId, hostId, operationId: pending.id, version: pending.version })} type="button">{t("agentSettings.cancelPending")}</button>}
		{pending && !canCancelAgentSettings(pending) && <p className="text-muted-foreground">{t("agentSettings.operationStarted")}</p>}
		{pending?.phase === "recovery_required" && !terminated && <button className="underline" onClick={() => { action.reset(); setConfirmation("stop"); }} type="button">{t("agentSettings.retryStop")}</button>}
		{snapshot.writerRecovery && <button className="underline" onClick={() => { action.reset(); setStopIfAlive(false); setConfirmation({ operationId: snapshot.writerRecovery!.operationId, version: snapshot.writerRecovery!.version }); }} type="button">{t("agentSettings.checkRetainedWriter")}</button>}
		{error && !confirmation && <p className="text-destructive [overflow-wrap:anywhere]" role="alert">{error}</p>}
		<ConfirmDialog open={confirmation !== undefined} title={t(confirmation === "stop" ? "agentSettings.stopTitle" : "agentSettings.checkWriterTitle")}
			description={t(confirmation === "stop" ? "agentSettings.stopDescription" : "agentSettings.checkWriterDescription")}
			confirmLabel={t(confirmation === "stop" ? "agentSettings.retryStop" : "agentSettings.checkWriter")}
			error={error} busy={action.isPending} onConfirm={() => { void confirm(); }} onOpenChange={(open) => { if (!open) setConfirmation(undefined); }}>
			{confirmation && confirmation !== "stop" && <label className="flex items-start gap-2 text-control"><input checked={stopIfAlive} disabled={action.isPending} onChange={(event) => setStopIfAlive(event.target.checked)} type="checkbox" />{t("agentSettings.allowStopWriter")}</label>}
		</ConfirmDialog>
	</div>;
}
