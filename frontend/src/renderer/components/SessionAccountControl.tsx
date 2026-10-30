import { AccountRecoveryAction } from "./AccountRecoveryAction";
import { useEffect } from "react";
import { useTranslation } from "react-i18next";
import {
	agentAccountProviderFor,
	useAgentAccountActions,
	useSessionAgentAccounts,
} from "../hooks/useAgentAccounts";
import {
	interfaceTransitionIsActive,
	interfaceTransitionIsAccountMove,
	useSessionInterfaceTransitionStatus,
} from "../hooks/useSessionInterfaceTransition";
import type { WorkspaceSession } from "../types/workspace";
import { SessionAccountSummary } from "./SessionAccountSummary";
import { Button } from "./ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "./ui/popover";

/** The label comes from the durable binding, never the current default. The
 * expanded view shares the inspector's query and move/limits controls. */
export function SessionAccountControl({ session }: { session: WorkspaceSession }) {
	const { t } = useTranslation();
	const provider = agentAccountProviderFor(session.provider);
	const current = useSessionAgentAccounts(provider ? session.id : undefined);
	const { transition, status } = useSessionInterfaceTransitionStatus(
		provider ? session.id : undefined,
	);
	const actions = useAgentAccountActions();
	const settled =
		transition && !interfaceTransitionIsActive(transition) ? transition.id + transition.phase : "";
	useEffect(() => {
		if (settled) void actions.refreshSession(session.id);
	}, [settled, session.id]);
	if (!provider || session.cloud) return null;
	const binding = current.data?.bindings?.[provider];
	const unavailable = Boolean(current.isError || (binding && !binding.available));
	const label = current.isError
		? t("settings.agentAccounts.providerUnavailable")
		: binding?.label ||
			(current.isLoading
				? t("settings.agentAccounts.bindingLoading")
				: t("settings.agentAccounts.bindingUnavailable"));
	return (
		<>
			{!session.isTerminated &&
			interfaceTransitionIsAccountMove(transition) &&
			status?.reasonCode === "TARGET_STOP_UNCONFIRMED" ? (
				<AccountRecoveryAction sessionId={session.id} />
			) : null}
			<Popover>
				<PopoverTrigger asChild>
					<Button
						type="button"
						variant="ghost"
						size="sm"
						aria-label={`${t("settings.agentAccounts.sessionAccount")}: ${label}`}
						title={label}
						className="max-w-48 min-w-0 text-xs"
					>
						<span className={`truncate ${unavailable ? "text-warning" : "text-muted-foreground"}`}>
							{label}
						</span>
					</Button>
				</PopoverTrigger>
				<PopoverContent align="end" className="w-80 max-w-[calc(100vw-2rem)]">
					<SessionAccountSummary session={session} />
				</PopoverContent>
			</Popover>
		</>
	);
}
