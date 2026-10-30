import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import { useTerminateSession } from "../hooks/useTerminateSession";
import type { WorkspaceSession } from "../types/workspace";
import { useAgentSettingsAction } from "../hooks/useSessionAgentSettings";
import { agentSettingsErrorMessage, agentSettingsQueryKey, canCancelAgentSettings, type AgentSettingsSnapshot } from "../lib/agent-settings";
import { ConfirmDialog } from "./ConfirmDialog";

/** Neither archiving nor an operator checkbox proves that a writer stopped. */
export function AgentSettingsRecovery({ snapshot, session, hostId }: {
	snapshot: AgentSettingsSnapshot; session: WorkspaceSession; hostId?: string;
}) {
	const { t } = useTranslation();
	const client = useQueryClient();
	const sessionId = session.id;
	const action = useAgentSettingsAction();
	const archive = useTerminateSession();
	const [confirmation, setConfirmation] = useState<"archive" | { operationId: string; version: number }>();
	const [stopIfAlive, setStopIfAlive] = useState(false);
	const failure = confirmation === "archive" ? archive.error : action.error;
	const error = failure ? agentSettingsErrorMessage(failure, t("agentSettings.conflict")) : undefined;
	const busy = action.isPending || archive.isPending;
	const pending = snapshot.pending;
	async function confirm() {
		if (!confirmation) return;
		try {
			if (confirmation === "archive") {
				try {
					await archive.mutateAsync({ ...session, hostId: hostId ?? session.hostId });
				} finally {
					void client.invalidateQueries({ queryKey: agentSettingsQueryKey(sessionId, hostId) });
				}
			} else {
				await action.mutateAsync({ kind: "settle", sessionId, hostId, ...confirmation, stopIfAlive });
			}
			setConfirmation(undefined);
		} catch { /* Keep the fence visible and allow a deliberate retry. */ }
	}
	return <div className="space-y-2">
		{pending && canCancelAgentSettings(pending) && <button className="underline" disabled={busy} onClick={() => action.mutate({ kind: "cancel", sessionId, hostId, operationId: pending.id, version: pending.version })} type="button">{t("agentSettings.cancelPending")}</button>}
		{pending && !canCancelAgentSettings(pending) && <p className="text-muted-foreground">{t("agentSettings.operationStarted")}</p>}
		{pending?.phase === "recovery_required" && pending.applyMode === "restart" && session.mode === "tui" && !session.isTerminated && <button className="underline" disabled={busy} onClick={() => { archive.reset(); setConfirmation("archive"); }} type="button">{t("shell.archiveSession")}</button>}
		{snapshot.writerRecovery && <button className="underline" disabled={busy} onClick={() => { action.reset(); setStopIfAlive(false); setConfirmation({ operationId: snapshot.writerRecovery!.operationId, version: snapshot.writerRecovery!.version }); }} type="button">{t("agentSettings.checkRetainedWriter")}</button>}
		{error && !confirmation && <p className="text-destructive [overflow-wrap:anywhere]" role="alert">{error}</p>}
		<ConfirmDialog open={confirmation !== undefined} title={t(confirmation === "archive" ? "archive.dialog" : "agentSettings.checkWriterTitle")}
			description={t(confirmation === "archive" ? "agentSettings.archiveRecoveryDescription" : "agentSettings.checkWriterDescription")}
			confirmLabel={t(confirmation === "archive" ? "shell.archiveSession" : "agentSettings.checkWriter")}
			error={error} busy={busy} onConfirm={() => { void confirm(); }} onOpenChange={(open) => { if (!open) setConfirmation(undefined); }}>
			{confirmation && confirmation !== "archive" && <label className="flex items-start gap-2 text-control"><input checked={stopIfAlive} disabled={busy} onChange={(event) => setStopIfAlive(event.target.checked)} type="checkbox" />{t("agentSettings.allowStopWriter")}</label>}
		</ConfirmDialog>
	</div>;
}
