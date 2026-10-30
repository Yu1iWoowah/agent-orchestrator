import { useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useChangeAgentSettings, type AgentSettingsChangeIntent } from "../hooks/useSessionAgentSettings";
import { apiErrorCode } from "../lib/api-client";
import { agentSettingsErrorMessage, settingsDraftPatch, type AgentSettingsSelection, type AgentSettingsSnapshot } from "../lib/agent-settings";
import { ConfirmDialog } from "./ConfirmDialog";
import { OptionMenu, OptionMenuContent, OptionMenuItem, OptionMenuTrigger } from "./ui/option-menu";

function SettingMenu({ label, value, choices, disabled, onChange }: {
	label: string; value: string; choices: readonly string[]; disabled: boolean; onChange: (value: string) => void;
}) {
	const { t } = useTranslation();
	return <div className="min-w-0 space-y-1">
		<p className="text-caption text-settings-muted">{label}</p>
		<OptionMenu modal>
			<OptionMenuTrigger aria-label={`${label}: ${value || t("agentSettings.providerDefault")}`} className="w-full justify-between" disabled={disabled}>
				<span className="truncate">{value || t("agentSettings.providerDefault")}</span>
			</OptionMenuTrigger>
			<OptionMenuContent className="max-h-64 max-w-[calc(100vw-32px)] overflow-y-auto">
				{["", ...choices].map((choice) => <OptionMenuItem key={choice} onSelect={() => onChange(choice)}>
					<span className="min-w-0 whitespace-normal [overflow-wrap:anywhere]">{choice || t("agentSettings.providerDefault")}</span>
				</OptionMenuItem>)}
			</OptionMenuContent>
		</OptionMenu>
	</div>;
}

/** Draft state starts only on an explicit edit. CDC updates never reset it. */
export function AgentSettingsEditor({ snapshot, sessionId, hostId }: {
	snapshot: AgentSettingsSnapshot; sessionId: string; hostId?: string;
}) {
	const { t } = useTranslation();
	const change = useChangeAgentSettings();
	const [draft, setDraft] = useState<{ selection: AgentSettingsSelection; baseline: AgentSettingsSelection; revision: number }>();
	const [open, setOpen] = useState(false);
	const [reviewRestart, setReviewRestart] = useState(false);
	const [stopTurn, setStopTurn] = useState(false);
	const intent = useRef<AgentSettingsChangeIntent | undefined>(undefined);
	const patch = draft ? settingsDraftPatch(draft.baseline, draft.selection) : {};
	const dirty = Object.keys(patch).length > 0;
	const error = change.error ? agentSettingsErrorMessage(change.error, t("agentSettings.conflict")) : undefined;
	const restart = Boolean(snapshot.restartRequired);
	const model = snapshot.capabilities.models.find((candidate) => candidate.model === draft?.selection.model || (!draft?.selection.model && candidate.isDefault));
	function edit(field: keyof AgentSettingsSelection, value: string) {
		if (!draft) return;
		intent.current = undefined;
		change.reset();
		setDraft({ ...draft, selection: { ...draft.selection, [field]: value } });
	}
	async function submit() {
		if (!draft || !dirty) return;
		if (restart && !reviewRestart) { setReviewRestart(true); return; }
		intent.current ??= { sessionId, hostId, key: crypto.randomUUID(), body: {
			expectedRevision: draft.revision, settings: patch, applyMode: restart ? "restart" : "after-turn", stopTurn: restart && stopTurn,
		} };
		try {
			await change.mutateAsync(intent.current);
			setOpen(false);
		} catch { /* Keep the captured draft, revision and intent for explicit retry. */ }
	}
	return <>
		<button className="rounded-md border border-border px-2 py-1.5 text-xs hover:bg-interactive-hover focus-visible:outline-2 focus-visible:outline-accent/50" type="button" onClick={() => {
			setDraft({ selection: { ...snapshot.settings }, baseline: { ...snapshot.settings }, revision: snapshot.revision });
			intent.current = undefined;
			setReviewRestart(false); setStopTurn(false); change.reset(); setOpen(true);
		}}>{t("agentSettings.edit")}</button>
		<ConfirmDialog open={open} title={t(reviewRestart ? "agentSettings.restartTitle" : "agentSettings.changeTitle")}
			description={t(reviewRestart ? "agentSettings.restartDescription" : "agentSettings.changeDescription")}
			confirmLabel={t(change.error && !apiErrorCode(change.error) ? "agentSettings.retry" : reviewRestart ? "agentSettings.confirmRestart" : restart ? "agentSettings.reviewRestart" : "agentSettings.apply")}
			confirmDisabled={!dirty} busy={change.isPending} error={error} onConfirm={() => { void submit(); }} onOpenChange={setOpen}>
			{draft && <div className="min-w-0 space-y-3">
				{draft.revision !== snapshot.revision && <div className="space-y-2 text-caption">
					<p className="text-warning" role="status">{t("agentSettings.draftChanged")}</p>
					<p className="text-settings-muted [overflow-wrap:anywhere]">{t("agentSettings.current")}: {snapshot.settings.model || t("agentSettings.providerDefault")} / {snapshot.settings.effort || t("agentSettings.providerDefault")} / fast {snapshot.settings.fastMode}</p>
					<button className="underline" disabled={change.isPending} onClick={() => {
						const changes = settingsDraftPatch(draft.baseline, draft.selection);
						setDraft({ baseline: { ...snapshot.settings }, revision: snapshot.revision, selection: {
							model: changes.model === undefined ? snapshot.settings.model : changes.model ?? "",
							effort: changes.effort === undefined ? snapshot.settings.effort : changes.effort ?? "",
							fastMode: changes.fastMode ?? snapshot.settings.fastMode,
						} });
						intent.current = undefined; change.reset();
					}} type="button">{t("agentSettings.useLatestRevision")}</button>
				</div>}
				{reviewRestart ? <label className="flex items-start gap-2 text-control">
					<input checked={stopTurn} disabled={change.isPending} onChange={(event) => { setStopTurn(event.target.checked); intent.current = undefined; change.reset(); }} type="checkbox" />
					{t("agentSettings.allowStopTurn")}
				</label> : <>
					<SettingMenu label={t("agentSettings.model")} value={draft.selection.model} choices={snapshot.capabilities.models.map((candidate) => candidate.model)} disabled={change.isPending} onChange={(value) => edit("model", value)} />
					<SettingMenu label={t("agentSettings.effort")} value={draft.selection.effort} choices={model?.efforts ?? []} disabled={change.isPending} onChange={(value) => edit("effort", value)} />
					<SettingMenu label={t("agentSettings.fast")} value={draft.selection.fastMode === "default" ? "" : draft.selection.fastMode} choices={["off", "on"]} disabled={change.isPending} onChange={(value) => edit("fastMode", value || "default")} />
					{snapshot.capabilities.state !== "supported" && <p className="text-caption text-settings-muted">{t("agentSettings.capabilitiesUnavailable")} · {snapshot.capabilities.reason}</p>}
				</>}
			</div>}
		</ConfirmDialog>
	</>;
}
