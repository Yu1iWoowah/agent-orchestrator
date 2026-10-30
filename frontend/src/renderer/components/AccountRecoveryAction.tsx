import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { agentAccountsQueryKey } from "../hooks/useAgentAccounts";
import { sessionInterfaceTransitionQueryKey } from "../hooks/useSessionInterfaceTransition";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button } from "./ui/button";

export function AccountRecoveryAction({ sessionId }: { sessionId: string }) {
	const { t } = useTranslation();
	const client = useQueryClient();
	const [open, setOpen] = useState(false);
	const [pending, setPending] = useState(false);
	const [error, setError] = useState("");
	const retry = async () => {
		setPending(true);
		setError("");
		try {
			const response = await apiClient.POST("/api/v1/sessions/{sessionId}/account-recovery", {
				params: { path: { sessionId } },
			});
			if (response.error) throw new Error(apiErrorMessage(response.error));
			if (!response.data?.confirmed) {
				setError(t("settings.agentAccounts.recoveryFailed"));
				return;
			}
			setOpen(false);
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : t("settings.agentAccounts.recoveryFailed"));
		} finally {
			await Promise.all([
				client.invalidateQueries({ queryKey: sessionInterfaceTransitionQueryKey(sessionId) }),
				client.invalidateQueries({ queryKey: agentAccountsQueryKey }),
			]);
			setPending(false);
		}
	};
	return (
		<>
			<Button
				type="button"
				size="sm"
				variant="ghost"
				className="max-w-full text-xs text-warning"
				onClick={() => {
					setError("");
					setOpen(true);
				}}
			>
				{t("settings.agentAccounts.recoveryTitle")}
			</Button>
			<ConfirmDialog
				open={open}
				title={t("settings.agentAccounts.recoveryTitle")}
				description={t("settings.agentAccounts.recoveryDescription")}
				confirmLabel={
					pending
						? t("settings.agentAccounts.recoveryPending")
						: t("settings.agentAccounts.recoveryAction")
				}
				busy={pending}
				error={error}
				onConfirm={() => void retry()}
				onOpenChange={setOpen}
			/>
		</>
	);
}
