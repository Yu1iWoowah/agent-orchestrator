import { ChevronDown, LoaderCircle } from "lucide-react";
import { useId, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { useSessionAgentSettings } from "../hooks/useSessionAgentSettings";
import { apiErrorMessage, hasTrustedApiBaseUrl } from "../lib/api-client";
import { useAgentAccountLabel } from "../hooks/useAgentAccounts";
import { verifiedSettingsActor, type AgentSettingsSelection } from "../lib/agent-settings";
import type { WorkspaceSession } from "../types/workspace";
import { AgentSettingsEditor } from "./AgentSettingsEditor";
import { AgentSettingsRecovery } from "./AgentSettingsRecovery";
import { AgentSettingsFailure } from "./AgentSettingsFailure";
import { isLocal } from "../lib/hosts";

function SettingsValue({ label, children }: { label: string; children: ReactNode }) {
	return <div className="grid min-w-0 grid-cols-[5rem_minmax(0,1fr)] gap-2">
		<dt className="text-muted-foreground">{label}</dt>
		<dd className="min-w-0 [overflow-wrap:anywhere]">{children}</dd>
	</div>;
}

function SelectionGroup({ label, settings }: { label: string; settings: AgentSettingsSelection }) {
	const { t } = useTranslation();
	return <div aria-label={label} className="min-w-0 space-y-1.5" role="group">
		<h4 className="font-medium">{label}</h4>
		<dl className="space-y-1">
			<SettingsValue label={t("agentSettings.model")}>{settings.model || t("agentSettings.providerDefault")}</SettingsValue>
			<SettingsValue label={t("agentSettings.effort")}>{settings.effort || t("agentSettings.providerDefault")}</SettingsValue>
			<SettingsValue label={t("agentSettings.fast")}>{settings.fastMode}</SettingsValue>
		</dl>
	</div>;
}

function EffectiveValue({ observation, label }: { observation: components["schemas"]["AgentSettingObservation"]; label: string }) {
	const { t } = useTranslation();
	return <><span>
		{observation.state === "known" ? observation.value || t("agentSettings.providerDefault")
			: observation.state === "not_provided" ? t("agentSettings.notProvided") : t("agentSettings.unknown")}
	</span>{observation.reason && <details className="mt-1 text-muted-foreground">
		<summary className="cursor-pointer underline">{t("agentSettings.observationReason", { field: label })}</summary>
		<p className="mt-1 [overflow-wrap:anywhere]">{observation.reason}</p>
	</details>}</>;
}

/** A saved preference is never substituted for provider confirmation. The
 * disclosure stays mounted across CDC refreshes so opening it cannot reset Chat. */
export function SessionAgentSettings({ session, hostId }: { session: WorkspaceSession; hostId?: string }) {
	const { t } = useTranslation();
	const [open, setOpen] = useState(false);
	const panelId = useId();
	const supported = !session.cloud && (session.provider === "codex" || session.provider === "claude-code");
	const query = useSessionAgentSettings({ sessionId: session.id, hostId, enabled: supported && open });
	const remote = Boolean(hostId && !isLocal(hostId));
	const accountLabel = useAgentAccountLabel(supported && open && !remote && hasTrustedApiBaseUrl());
	if (!supported) return null;
	const snapshot = query.data;
	return <div className="mt-3 min-w-0 border-t border-(--color-border-settings-input) pt-3 text-xs" data-testid="session-agent-settings">
		<button aria-controls={panelId} aria-expanded={open} className="flex w-full items-center justify-between gap-2 py-1 text-left font-medium focus-visible:outline-2 focus-visible:outline-accent/50" onClick={() => setOpen(!open)} type="button">
			{t("agentSettings.title")}<ChevronDown aria-hidden className="size-3.5 shrink-0" />
		</button>
		{open && <section aria-label={t("agentSettings.title")} className="mt-3 min-w-0 space-y-4" id={panelId}>
			{query.isPending && !snapshot && <div className="flex items-center gap-2 text-muted-foreground" role="status"><LoaderCircle aria-hidden className="size-3 animate-spin" />{t("agentSettings.loading")}</div>}
			{query.error && <div className="space-y-1 text-destructive" role="alert">
				<p>{apiErrorMessage(query.error, t("agentSettings.loadError"))}</p>
				<button className="underline" onClick={() => { void query.refetch(); }} type="button">{t("agentSettings.retry")}</button>
			</div>}
			{snapshot && <>
				{!session.isTerminated && <AgentSettingsEditor hostId={hostId} key={`${hostId ?? "local"}:${session.id}`} sessionId={session.id} snapshot={snapshot} />}
				<AgentSettingsFailure hostId={hostId} sessionId={session.id} snapshot={snapshot} terminated={session.isTerminated === true} />
				<SelectionGroup label={t("agentSettings.current")} settings={snapshot.settings} />
				<dl className="space-y-1 text-muted-foreground">
					<SettingsValue label={t("agentSettings.account")}>{snapshot.capabilities.accountId ? remote ? snapshot.capabilities.accountId : accountLabel(snapshot.capabilities.accountId) : t("agentSettings.unknown")}</SettingsValue>
					<SettingsValue label={t("agentSettings.restart")}>{t(snapshot.restartRequired ? "agentSettings.required" : "agentSettings.nextTurn")}</SettingsValue>
				</dl>
				{session.spawnAgentSettings && <details className="min-w-0 text-muted-foreground">
					<summary className="cursor-pointer">{t("agentSettings.spawnSources")}</summary>
					<dl className="mt-2 space-y-1">{Object.entries(session.spawnAgentSettings.sources).map(([field, source]) => <SettingsValue key={field} label={field}>{source}</SettingsValue>)}</dl>
					<p>{session.spawnAgentSettings.validation}{session.spawnAgentSettings.reason ? ` · ${session.spawnAgentSettings.reason}` : ""}</p>
				</details>}
				{snapshot.pending && <>
					<SelectionGroup label={snapshot.pending.applyMode === "restart" ? t("agentSettings.pendingRestart") : t("agentSettings.pending")} settings={snapshot.pending.target} />
					{verifiedSettingsActor(snapshot.pending) && <p className="text-muted-foreground [overflow-wrap:anywhere]">{t("agentSettings.byOrchestrator", { id: verifiedSettingsActor(snapshot.pending) })}</p>}
				</>}
				<div aria-label={t("agentSettings.effective")} className="space-y-1.5" role="group">
					<h4 className="font-medium">{t("agentSettings.effective")}</h4>
					<dl className="space-y-1">
						<SettingsValue label={t("agentSettings.model")}><EffectiveValue label={t("agentSettings.model")} observation={snapshot.effective.model} /></SettingsValue>
						<SettingsValue label={t("agentSettings.effort")}><EffectiveValue label={t("agentSettings.effort")} observation={snapshot.effective.effort} /></SettingsValue>
						<SettingsValue label={t("agentSettings.fast")}><EffectiveValue label={t("agentSettings.fast")} observation={snapshot.effective.fastMode} /></SettingsValue>
					</dl>
				</div>
				{snapshot.writerRecovery && <p className="text-warning [overflow-wrap:anywhere]" role="alert">{t("agentSettings.writerUnconfirmed")}</p>}
				<AgentSettingsRecovery hostId={hostId} key={`recovery:${hostId ?? "local"}:${session.id}`} session={session} snapshot={snapshot} />
				{Boolean(snapshot.history?.length) && <details className="min-w-0">
					<summary className="cursor-pointer font-medium">{t("agentSettings.history")}</summary>
					<ol className="mt-2 divide-y divide-border">
						{snapshot.history?.map((entry) => <li className="space-y-1 py-2 [overflow-wrap:anywhere]" key={entry.id}>
							<p>{verifiedSettingsActor(entry) ? t("agentSettings.byOrchestrator", { id: verifiedSettingsActor(entry) }) : t(entry.actor.kind === "local_operator_unverified" ? "agentSettings.localOperator" : "agentSettings.unverifiedSource")}</p>
							<p className="text-muted-foreground">{entry.phase} · {entry.updatedAt}</p>
							<p>{entry.source.model || t("agentSettings.providerDefault")} / {entry.source.effort || t("agentSettings.providerDefault")} / {t("agentSettings.fast")} {entry.source.fastMode} → {entry.target.model || t("agentSettings.providerDefault")} / {entry.target.effort || t("agentSettings.providerDefault")} / {t("agentSettings.fast")} {entry.target.fastMode}</p>
							{entry.errorCode && <p>{entry.errorCode}</p>}
							{entry.writerStatus && <p>{t(entry.writerStatus === "stopped" ? "agentSettings.writerStopped" : "agentSettings.writerUnconfirmed")}</p>}
						</li>)}
					</ol>
				</details>}
			</>}
		</section>}
	</div>;
}
