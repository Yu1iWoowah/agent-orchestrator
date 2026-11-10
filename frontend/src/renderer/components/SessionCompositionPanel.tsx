import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { useSessionComposition, type SessionComposition } from "../hooks/useSessionComposition";
import { Button } from "./ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "./ui/select";
import { SettingsSection } from "./settings/SettingsSection";

const categories = [
	"system_developer",
	"tool_schema",
	"conversation_history",
	"tool_result",
	"retained_thinking",
	"other_observed",
] as const;
const reasons = [
	"hidden_thinking",
	"unknown_fields",
	"missing_key",
	"compaction_gap",
	"incomplete_tail",
	"window_head_gap",
	"record_limit",
	"parse_limit",
	"source_truncated",
	"unknown_schema",
	"unsupported_source_version",
	"source_read_failed",
	"source_identity_changed",
	"policy_changed",
	"canceled",
	"storage_low_space",
	"storage_write_failed",
	"storage_read_failed",
	"session_limit",
	"host_limit",
	"disabled",
	"analysis_not_requested",
	"analysis_running",
] as const;
const fixedReasonCodes = [
	"ambiguous_source",
	"analysis_failed",
	"fork_context_unknown",
	"invalid_record",
	"invalid_snapshot",
	"native_generation_unknown",
	"native_identity_unknown",
	"no_observed_content",
	"projection_limit",
	"retained_context_unknown",
	"scope_not_allowed",
	"scope_root_mismatch",
	"source_enumeration_incomplete",
	"source_identity_mismatch",
	"source_metadata_failed",
	"source_missing",
	"source_version_unknown",
	"storage_identity_changed",
	"storage_space_unknown",
	"storage_unavailable",
	"subagent_source",
	"unsupported_content",
] as const;
const reasonKey = (value: string) => {
	const reason = reasons.find((item) => item === value);
	return reason ? (`rca.reason.${reason}` as const) : "rca.status.unknown";
};
const reasonText = (value: string, translate: TFunction) => fixedReasonCodes.find((code) => code === value) ?? translate(reasonKey(value));
const number = (value: number | null | undefined) =>
	typeof value === "number" && Number.isFinite(value) && value >= 0 ? value.toLocaleString() : "UNKNOWN";
const quality = (value: string) => (["ready", "partial", "running", "unavailable"] as const).find((item) => item === value) ?? "unknown";

function Snapshot({ snapshot }: { snapshot: NonNullable<SessionComposition["recorded"]>["snapshots"][number] }) {
	const { t } = useTranslation();
	const [turn, setTurn] = useState("session");
	const buckets =
		turn === "session"
			? snapshot.buckets
			: turn === "unpaired"
				? snapshot.unpaired
				: snapshot.turns.find((item) => String(item.ordinal) === turn)?.buckets;
	return (
		<>
			<div className="flex flex-wrap items-center gap-2">
				<Select value={turn} onValueChange={setTurn}>
					<SelectTrigger aria-label={t("rca.granularity")} size="sm">
						<SelectValue />
					</SelectTrigger>
					<SelectContent>
						<SelectItem value="session">{t("rca.sessionSnapshot")}</SelectItem>
						<SelectItem value="unpaired">{t("rca.unpaired")}</SelectItem>
						{snapshot.turns.map((item) => (
							<SelectItem key={item.ordinal} value={String(item.ordinal)}>
								{t("rca.turn", { number: number(item.ordinal) })}
							</SelectItem>
						))}
					</SelectContent>
				</Select>
				<span>{t(`rca.status.${quality(snapshot.status)}`)}</span>
			</div>
			<p>
				{t("rca.coverage", {
					bytes: number(snapshot.analyzedBytes),
					start: number(snapshot.startOffset),
					end: number(snapshot.endOffset),
					records: number(snapshot.records),
				})}
			</p>
			<p>
				{t("rca.references", {
					source: number(snapshot.usageSourceId),
					binding: number(snapshot.bindingId),
					generation: number(snapshot.sourceGeneration),
				})}
			</p>
			<p>
				{t("rca.method")} · {snapshot.sourceVersion === "0.167.0" ? "0.167.0" : "UNKNOWN"}
			</p>
			<p>{t("rca.turnScope")}</p>
			{snapshot.reasons.length > 0 ? (
				<ul className="list-disc pl-5">
					{[...new Set(snapshot.reasons)].map((reason, index) => (
						<li key={index}>{reasonText(reason, t)}</li>
					))}
				</ul>
			) : null}
			<div className="overflow-x-auto">
				<table className="w-full text-left text-xs" aria-label={t("rca.buckets")}>
					<thead>
						<tr className="border-b border-border">
							<th className="py-2 font-medium">{t("rca.category")}</th>
							<th className="px-2 text-right font-medium">{t("rca.bytes")}</th>
							<th className="text-right font-medium">{t("rca.estimate")}</th>
						</tr>
					</thead>
					<tbody>
						{categories.map((category) => {
							const bucket = buckets?.find((item) => item.category === category);
							return (
								<tr className="border-b border-border last:border-0" key={category}>
									<th className="py-2 font-normal">{t(`rca.category.${category}`)}</th>
									<td className="px-2 text-right tabular-nums">{number(bucket?.bytes)}</td>
									<td className="text-right tabular-nums">{number(bucket?.estimatedTokens)}</td>
								</tr>
							);
						})}
					</tbody>
				</table>
			</div>
		</>
	);
}

export function SessionCompositionPanel({ sessionId }: { sessionId: string }) {
	const { t } = useTranslation();
	const { view, pending, run } = useSessionComposition(sessionId);
	const data = view.status === "ready" ? view.data : null;
	const policy = data?.policy;
	const [source, setSource] = useState("0");
	const snapshots =
		policy?.effectiveEnabled && !policy.emergencyStop && data?.status !== "unavailable" ? data?.recorded?.snapshots : undefined;
	const snapshot = snapshots?.[Number(source)] ?? snapshots?.[0];
	return (
		<SettingsSection title={t("rca.title")} grouped testId="session-composition">
			<div className="settings-content-row space-y-3 p-4 text-xs text-muted-foreground" aria-busy={pending}>
				<p>{t("rca.scope")}</p>
				<div className="flex flex-wrap items-center gap-2">
					<Select
						disabled={pending}
						value={policy?.session === true ? "on" : policy?.session === false ? "off" : "inherit"}
						onValueChange={(value) => {
							if (value === "on" || value === "off" || value === "inherit") void run(value);
						}}
					>
						<SelectTrigger aria-label={t("rca.policy")} size="sm">
							<SelectValue />
						</SelectTrigger>
						<SelectContent>
							{(["inherit", "on", "off"] as const).map((value) => (
								<SelectItem key={value} value={value}>
									{t(`rca.policy.${value}`)}
								</SelectItem>
							))}
						</SelectContent>
					</Select>
					<Button
						disabled={pending || !policy?.effectiveEnabled || policy.emergencyStop}
						size="sm"
						variant="outline"
						onClick={() => {
							void run("analyze");
						}}
					>
						{t("rca.analyze")}
					</Button>
					<Button
						disabled={pending}
						size="sm"
						variant="ghost"
						onClick={() => {
							void run("refresh");
						}}
					>
						{t("rca.refresh")}
					</Button>
					<Button
						disabled={pending}
						size="sm"
						variant="ghost"
						onClick={() => {
							void run("off");
						}}
					>
						{t("rca.cancel")}
					</Button>
					<Button
						disabled={pending}
						size="sm"
						variant="ghost"
						onClick={() => {
							void run("purge");
						}}
					>
						{t("rca.purge")}
					</Button>
				</div>
				{view.status === "loading" ? <p role="status">{t("common.loading")}</p> : null}
				{view.status === "error" ? <p role="alert">{t("rca.error")}</p> : null}
				{data ? (
					<>
						<p role="status">
							{t(`rca.status.${quality(data.status)}`)}
							{data.reason ? ` · ${reasonText(data.reason, t)}` : ""}
						</p>
						<p>
							{t("rca.parents", {
								project: policy?.project == null ? t("rca.policy.inherit") : t(policy.project ? "rca.policy.on" : "rca.policy.off"),
								role: policy?.role == null ? t("rca.policy.inherit") : t(policy.role ? "rca.policy.on" : "rca.policy.off"),
								effective: t(policy?.effectiveEnabled ? "rca.policy.on" : "rca.policy.off"),
							})}
						</p>
						{snapshots && snapshots.length > 1 ? (
							<Select value={source} onValueChange={setSource}>
								<SelectTrigger aria-label={t("rca.source")} size="sm">
									<SelectValue />
								</SelectTrigger>
								<SelectContent>
									{snapshots.map((_, index) => (
										<SelectItem value={String(index)} key={index}>
											{t("rca.sourceNumber", { number: index + 1 })}
										</SelectItem>
									))}
								</SelectContent>
							</Select>
						) : null}
						{snapshot ? (
							<Snapshot key={`${snapshot.scopeId}:${data.recorded?.scopeRevision}`} snapshot={snapshot} />
						) : (
							<p>{t("rca.noSnapshot")}</p>
						)}
						<p>{t("rca.requestUnknown")}</p>
						<h4 className="font-medium text-foreground">{t("rca.canonical")}</h4>
						{data.canonical.events.length === 0 ? (
							<p>{t("rca.emptyCanonical")}</p>
						) : (
							<div className="overflow-x-auto">
								<table className="w-full text-left" aria-label={t("rca.canonical")}>
									<thead>
										<tr>
											<th>{t("rca.event")}</th>
											<th>{t("rca.input")}</th>
											<th>{t("rca.output")}</th>
											<th>{t("rca.cached")}</th>
										</tr>
									</thead>
									<tbody>
										{data.canonical.events.map((event) => (
											<tr key={event.id}>
												<td className="py-2">{number(event.id)}</td>
												<td>{number(event.inputTokens)}</td>
												<td>{number(event.outputTokens)}</td>
												<td>{number(event.cachedInputTokens)}</td>
											</tr>
										))}
									</tbody>
								</table>
							</div>
						)}
						{data.canonical.hasMore ? <p>{t("rca.moreCanonical")}</p> : null}
						<h4 className="font-medium text-foreground">{t("rca.baseline")}</h4>
						<p>{t("rca.baselineNumbers", { bytes: number(data.baseline.bytes), tokens: number(data.baseline.estimatedTokens) })}</p>
					</>
				) : null}
				<div className="flex flex-wrap items-center gap-2 border-t border-border pt-3">
					<Button
						disabled={pending}
						size="sm"
						variant="ghost"
						onClick={() => {
							void run(policy?.emergencyStop ? "resume" : "stop");
						}}
					>
						{t(policy?.emergencyStop ? "rca.resumeHost" : "rca.stopHost")}
					</Button>
					<span>{t("rca.hostScope")}</span>
				</div>
			</div>
		</SettingsSection>
	);
}
