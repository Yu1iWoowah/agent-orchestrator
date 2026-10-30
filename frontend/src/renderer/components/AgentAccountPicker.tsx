import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import {
	agentAccountName,
	agentAccountProviderFor,
	useAgentAccounts,
} from "../hooks/useAgentAccounts";
import { agentModelsQueryOptions } from "../hooks/useAgentModelsQuery";
import { localAccountModelScope, type LocalAccountRole } from "../lib/local-account-model-scope";
import { SettingsOptionMenu } from "./settings/SettingsOptionMenu";

/** Values are only explicit references. The daemon owns inheritance; catalog
 * updates change the annotation, never the user's draft or retry payload. */
export function AgentAccountPicker({
	harness,
	value,
	onChange,
	projectId = "",
	role = "worker",
	disabled = false,
	inheritId,
	label,
}: {
	harness: string;
	value: string;
	onChange: (value: string) => void;
	projectId?: string;
	role?: LocalAccountRole;
	disabled?: boolean;
	inheritId?: string;
	label?: string;
}) {
	const { t } = useTranslation();
	const provider = agentAccountProviderFor(harness);
	const accounts = useAgentAccounts(Boolean(provider));
	const catalog = useQuery({
		...agentModelsQueryOptions(
			harness,
			localAccountModelScope(inheritId === undefined ? projectId : "", role),
			undefined,
			true,
		),
		enabled: Boolean(provider) && !inheritId,
	});
	if (!provider) return null;
	const rows = (accounts.data ?? []).filter((a) => a.provider === provider);
	const binding = catalog.data?.accountBinding;
	const inherited = rows.find((a) => a.id === (inheritId || binding?.accountId));
	const inheritedName = inherited ? agentAccountName(inherited, t) : undefined;
	const unavailable = value !== "" && !rows.some((a) => a.id === value);
	const options = [
		{
			value: "",
			label: inheritedName
				? t("settings.agentAccounts.inheritAccount", {
						name: inheritedName,
						source: t(
							`settings.agentAccounts.source.${inheritId ? "project" : (binding?.source ?? "global")}`,
						),
					})
				: t("settings.agentAccounts.inherit"),
		},
		...rows.map((a) => ({
			value: a.id,
			label:
				agentAccountName(a, t) +
				(!a.signedIn && !a.device ? ` · ${t("settings.agentAccounts.signedOut")}` : ""),
			disabled: !a.device && !a.signedIn,
		})),
		...(unavailable
			? [{ value, label: t("settings.agentAccounts.removedAccount"), disabled: true }]
			: []),
	];
	return (
		<div className="min-w-0 max-w-full" data-testid="agent-account-picker">
			<SettingsOptionMenu
				aria-label={label ?? t("settings.agentAccounts.sessionAccount")}
				value={value}
				options={options}
				disabled={disabled || (accounts.isLoading && !accounts.data)}
				onChange={onChange}
				triggerClassName="max-w-64"
			/>
			{accounts.isError || catalog.isError ? (
				<p role="alert" className="text-xs text-error">
					{t("settings.agentAccounts.providerUnavailable")}
				</p>
			) : null}
			{unavailable && !accounts.isLoading ? (
				<p role="status" className="text-xs text-warning">
					{t("settings.agentAccounts.selectionUnavailable")}
				</p>
			) : null}
		</div>
	);
}
