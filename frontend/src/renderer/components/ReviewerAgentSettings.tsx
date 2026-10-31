import { useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { usePinReviewerAgentSettings, useReviewerAgentSettings, type ReviewerSettingsIntent } from "../hooks/useReviewerAgentSettings";
import { agentSettingsErrorMessage, settingsDraftPatch, type AgentSettingsSelection } from "../lib/agent-settings";
import { apiErrorMessage } from "../lib/api-client";
import { ConfirmDialog } from "./ConfirmDialog";

type Draft = { baseline: AgentSettingsSelection; selection: AgentSettingsSelection; revision: number };

/** A pin is a future pass preference. Live provider-effective values stay unknown. */
export function ReviewerAgentSettings({ reviewId, hostId }: { reviewId: string; hostId?: string }) {
	const { t } = useTranslation();
	const query = useReviewerAgentSettings(reviewId, hostId);
	const change = usePinReviewerAgentSettings();
	const [draft, setDraft] = useState<Draft>();
	const intent = useRef<ReviewerSettingsIntent | undefined>(undefined);
	const data = query.data;
	const patch = draft ? settingsDraftPatch(draft.baseline, draft.selection) : {};
	if (draft && patch.model !== undefined) patch.effort = draft.selection.effort || null;
	const dirty = Object.keys(patch).length > 0;
	const error = change.error ? agentSettingsErrorMessage(change.error, t("reviewerSettings.conflict")) : undefined;
	const primary = data?.harness === "codex" || data?.harness === "claude-code";
	function edit(field: keyof AgentSettingsSelection, value: string) {
		if (!draft) return;
		// Only values from the closed fast vocabulary can enter the preference.
		if (field === "fastMode" && value !== "default" && value !== "on" && value !== "off") return;
		intent.current = undefined;
		change.reset();
		setDraft({ ...draft, selection: { ...draft.selection, ...(field === "model" ? { effort: "" } : {}), [field]: value } });
	}
	function begin() {
		if (!data?.settings) return;
		intent.current = undefined;
		change.reset();
		setDraft({ baseline: { ...data.settings }, selection: { ...data.settings }, revision: data.revision });
	}
	async function submit() {
		if (!draft || !dirty || data?.activePass) return;
		intent.current ??= { reviewId, hostId, key: crypto.randomUUID(), body: { expectedRevision: draft.revision, settings: patch } };
		try { await change.mutateAsync(intent.current); setDraft(undefined); } catch { /* The captured intent and draft stay available for explicit retry. */ }
	}
	return <div className="space-y-1 rounded-md border border-border p-2 text-xs [overflow-wrap:anywhere]" aria-label={t("reviewerSettings.nextPass")}>
		<p className="font-medium">{t("reviewerSettings.nextPass")}</p>
		<p>{data?.settings ? `${data.settings.model || t("agentSettings.providerDefault")} · ${data.settings.effort || t("agentSettings.providerDefault")} · fast: ${data.settings.fastMode}` : t("reviewerSettings.preferenceUnknown")}</p>
		<p className="text-muted-foreground">{t("reviewerSettings.effectiveUnknown")}</p>
		{data?.activePass && <p role="status">{t("reviewerSettings.activePass")}</p>}
		{query.error && <p role="alert">{apiErrorMessage(query.error)}</p>}
		{query.error && <button type="button" onClick={() => void query.refetch()}>{t("reviewerSettings.reload")}</button>}
		<button type="button" className="rounded border border-border px-2 py-1 disabled:opacity-50" disabled={!data?.settings || data.activePass || !primary} onClick={begin}>{t("reviewerSettings.edit")}</button>
		<ConfirmDialog open={Boolean(draft)} title={t("reviewerSettings.nextPass")} description={t("reviewerSettings.description")} confirmLabel={t("reviewerSettings.pin")} busy={change.isPending} confirmDisabled={!dirty || Boolean(data?.activePass)} error={error} onConfirm={() => void submit()} onOpenChange={(open) => { if (!open) setDraft(undefined); }}>
			{draft && <div className="space-y-3">
				<label className="block space-y-1">{t("agentSettings.model")}<input className="w-full rounded border border-border bg-background p-2" value={draft.selection.model} onChange={(event) => edit("model", event.target.value)} disabled={change.isPending} /></label>
				<label className="block space-y-1">{t("agentSettings.effort")}<input className="w-full rounded border border-border bg-background p-2" value={draft.selection.effort} onChange={(event) => edit("effort", event.target.value)} disabled={change.isPending} /></label>
				<label className="block space-y-1">{t("agentSettings.fast")}<select className="w-full rounded border border-border bg-background p-2" value={draft.selection.fastMode} onChange={(event) => edit("fastMode", event.target.value)} disabled={change.isPending}>
					<option value="default">{t("agentSettings.providerDefault")}</option><option value="on">{t("reviewerSettings.fastOn")}</option><option value="off">{t("reviewerSettings.fastOff")}</option>
				</select></label>
				{draft.selection.fastMode === "on" && <p>{t("reviewerSettings.fastCost")}</p>}
				{data?.activePass && <p role="status">{t("reviewerSettings.activePass")}</p>}
				{error && <button type="button" disabled={change.isPending} onClick={() => { void query.refetch().then((fresh) => { if (fresh.data?.settings) { intent.current = undefined; change.reset(); setDraft({ baseline: { ...fresh.data.settings }, selection: { ...fresh.data.settings }, revision: fresh.data.revision }); } }); }}>{t("reviewerSettings.reload")}</button>}
			</div>}
		</ConfirmDialog>
	</div>;
}
