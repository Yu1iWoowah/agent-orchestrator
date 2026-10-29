import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import type { DiagnosticsStatus } from "../../shared/app-diagnostics";
import { aoBridge } from "../lib/bridge";

/** Read-only context next to problem reporting; consent/restart actions belong to PR3. */
export function AppDiagnosticsStatus() {
	const { t } = useTranslation();
	const [status, setStatus] = useState<DiagnosticsStatus>();
	useEffect(() => {
		const bridge = aoBridge.appDiagnostics;
		if (!bridge) return;
		let active = true;
		let pushed = false;
		const stop = bridge.onStatus((next) => {
			pushed = true;
			if (active) setStatus(next);
		});
		void bridge
			.getStatus()
			.then((next) => {
				if (active && !pushed) setStatus(next);
			})
			.catch(() => undefined);
		return () => {
			active = false;
			stop();
		};
	}, []);
	if (status?.reason !== "adopted_daemon") return null;
	return (
		<p role="status" className="text-sm text-settings-muted">
			{t("appDiagnostics.adoptedUnavailable")}
		</p>
	);
}
