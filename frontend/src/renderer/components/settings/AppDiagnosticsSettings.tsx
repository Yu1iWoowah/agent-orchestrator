import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import type { DiagnosticAudit, DiagnosticConsent, DiagnosticsStatus } from "../../../shared/app-diagnostics";
import { aoBridge } from "../../lib/bridge";
import { workspaceQueryOptions } from "../../hooks/useWorkspaceQuery";
import { Button } from "../ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { SettingsSection } from "./SettingsSection";
import { SettingsRow } from "./SettingsRow";

export function AppDiagnosticsSettings() {
	const { t } = useTranslation();
	const { data: workspaces } = useQuery(workspaceQueryOptions);
	const sessions = (workspaces ?? [])
		.filter((row) => !row.hostId && row.kind !== "cloud")
		.flatMap((row) =>
			row.sessions
				.filter((session) => session.kind === "orchestrator" && !session.isTerminated)
				.map((session) => ({ id: session.id, projectId: row.id })),
		);
	const [selected, setSelected] = useState("");
	const sessionId = sessions.some((row) => row.id === selected) ? selected : (sessions[0]?.id ?? "");
	const [status, setStatus] = useState<DiagnosticsStatus>();
	const [consent, setConsent] = useState<DiagnosticConsent | null>(null);
	const [audit, setAudit] = useState<DiagnosticAudit>();
	const [pending, setPending] = useState(false);
	const [error, setError] = useState("");
	const [now, setNow] = useState(Date.now());
	useEffect(() => {
		const bridge = aoBridge.appDiagnostics;
		if (!bridge) return;
		let active = true;
		let revision = 0;
		const refresh = async () => {
			const current = ++revision;
			try {
				const [next, grant] = await Promise.all([bridge.getStatus(), bridge.getConsent()]);
				if (active && current === revision) {
					setStatus(next);
					setConsent(grant);
					if (!grant) setAudit(undefined);
				}
			} catch {
				if (active) setError(t("appDiagnostics.failed"));
			}
		};
		const stop = bridge.onStatus(() => {
			void refresh();
		});
		void refresh();
		const timer = setInterval(() => {
			if (active) setNow(Date.now());
		}, 1000);
		return () => {
			active = false;
			stop();
			clearInterval(timer);
		};
	}, [t]);
	const identity = async () => {
		const document = await aoBridge.appDiagnostics.register();
		return {
			schemaVersion: 1 as const,
			requestId: crypto.randomUUID(),
			documentId: document.documentId,
			windowId: document.windowId,
			generation: document.generation,
		};
	};
	const action = async (kind: "enable" | "revoke" | "audit") => {
		setPending(true);
		setError("");
		try {
			const request = await identity();
			if (kind === "enable") {
				const result = await aoBridge.appDiagnostics.requestEnable({ ...request, sessionId });
				if (result.outcome === "enabled") setConsent({ grantId: result.grantId, sessionId: result.sessionId, expiresAt: result.expiresAt });
			} else if (kind === "revoke") {
				setConsent(null);
				setAudit(undefined);
				await aoBridge.appDiagnostics.revoke(request);
			} else setAudit(await aoBridge.appDiagnostics.audit(request));
		} catch {
			setError(t(kind === "revoke" ? "appDiagnostics.cleanupFailed" : "appDiagnostics.failed"));
		} finally {
			setPending(false);
		}
	};
	const unavailable = status?.state === "unavailable";
	const remaining = consent ? Math.max(0, Math.ceil((consent.expiresAt - now) / 1000)) : 0;

	const unavailableReasons: Partial<Record<DiagnosticsStatus["reason"], string>> = {
		adopted_daemon: t("appDiagnostics.adoptedUnavailable"),
		not_connected: t("appDiagnostics.notConnected"),
		no_window: t("appDiagnostics.noWindow"),
		renderer_unavailable: t("appDiagnostics.rendererUnavailable"),
		unsupported: t("appDiagnostics.unsupported"),
	};
	const reason = unavailable ? (unavailableReasons[status.reason] ?? t("appDiagnostics.unavailable")) : t("appDiagnostics.scope");

	return (
		<SettingsSection title={t("appDiagnostics.title")} grouped>
			<SettingsRow
				label={t("appDiagnostics.access")}
				description={
					consent ? (
						t("appDiagnostics.active", {
							session: consent.sessionId,
							minutes: Math.floor(remaining / 60),
							seconds: String(remaining % 60).padStart(2, "0"),
						})
					) : (
						<span role="status">{reason}</span>
					)
				}
			>
				{consent ? (
					<Button variant="ghost" disabled={pending} onClick={() => void action("revoke")}>
						{t("appDiagnostics.revoke")}
					</Button>
				) : (
					<Button variant="ghost" disabled={pending || !sessionId || !status || unavailable} onClick={() => void action("enable")}>
						{t("appDiagnostics.allow")}
					</Button>
				)}
			</SettingsRow>
			{!consent && (
				<SettingsRow label={t("appDiagnostics.orchestrator")}>
					<Select value={sessionId} onValueChange={setSelected} disabled={pending || unavailable || sessions.length === 0}>
						<SelectTrigger aria-label={t("appDiagnostics.orchestrator")} className="w-60 max-w-full">
							<SelectValue placeholder={t("appDiagnostics.noSessions")} />
						</SelectTrigger>
						<SelectContent>
							{sessions.map((row) => (
								<SelectItem key={row.id} value={row.id}>
									{row.id}
								</SelectItem>
							))}
						</SelectContent>
					</Select>
				</SettingsRow>
			)}
			{consent && (
				<SettingsRow label={t("appDiagnostics.audit")} description={consent.grantId}>
					<Button variant="ghost" disabled={pending} onClick={() => void action("audit")}>
						{t("appDiagnostics.viewAudit")}
					</Button>
				</SettingsRow>
			)}
			{error && (
				<div className="settings-content-row text-sm text-error" role="alert">
					{error}
				</div>
			)}
			{audit && (
				<div className="settings-content-row text-sm" aria-label={t("appDiagnostics.audit")}>
					{(audit.partial || audit.droppedCount > 0) && (
						<p role="status" className="text-settings-muted">
							{t("appDiagnostics.auditPartial", { count: audit.droppedCount })}
						</p>
					)}
					<table className="w-full text-left">
						<thead>
							<tr>
								<th>{t("appDiagnostics.time")}</th>
								<th>{t("appDiagnostics.result")}</th>
								<th>{t("appDiagnostics.bytes")}</th>
							</tr>
						</thead>
						<tbody>
							{audit.records.map((row, index) => (
								<tr key={index}>
									<td>{new Date(row.at).toLocaleTimeString()}</td>
									<td>
										{t(`appDiagnostics.outcomes.${row.outcome}`)}
										{row.partial ? ` · ${t("appDiagnostics.partial")}` : ""}
									</td>
									<td>{row.bytes}</td>
								</tr>
							))}
						</tbody>
					</table>
				</div>
			)}
		</SettingsSection>
	);
}
