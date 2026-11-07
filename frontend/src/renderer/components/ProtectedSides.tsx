import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button } from "./ui/button";
import { SettingsSection } from "./settings/SettingsSection";
import { SideCompactionNotice } from "./chat/SideCompactionNotice";

export function ProtectedSides() {
	const { t } = useTranslation();
	const client = useQueryClient();
	const [closeId, setCloseId] = useState<string>();
	const queryKey = ["protected-side-chats"];
	const sides = useQuery({ queryKey, refetchInterval: 3_000, queryFn: async () => {
		const { data, error } = await apiClient.GET("/api/v1/side-chats/protected");
		if (error) throw new Error(apiErrorMessage(error));
		return data.sides;
	} });
	const close = useMutation({ mutationFn: async (sideId: string) => {
		const { error } = await apiClient.DELETE("/api/v1/side-chats/protected/{sideId}", { params: { path: { sideId } } });
		if (error) throw new Error(apiErrorMessage(error));
	}, onSuccess: async () => {
		setCloseId(undefined);
		await client.invalidateQueries({ queryKey });
	} });
	return (
		<SettingsSection title={t("protectedSides.title")} sectionId="protectedSides">
			<p className="mb-4 text-sm text-muted-foreground">{t("protectedSides.description")}</p>
			{ sides.isPending ? <p>{t("protectedSides.loading")}</p> : sides.isError ? <p role="alert">{apiErrorMessage(sides.error)}</p> : sides.data.length === 0 ? <p className="text-sm text-muted-foreground">{t("protectedSides.empty")}</p> : (
				<ul className="divide-y divide-border">
					{sides.data.map((side) => <li key={side.sideId} className="space-y-3 py-4">
						<p className="text-sm font-medium">{side.sideId} · {side.provider}</p>
						<p className="text-xs text-muted-foreground">{side.sessionId}{side.orphan ? ` · ${t("protectedSides.orphan")}` : ""}</p>
						<SideCompactionNotice protection={side.protection} onClose={() => { close.reset(); setCloseId(side.sideId); }} busy={close.isPending} />
					</li>)}
				</ul>
			)}
			{ sides.isError ? <Button variant="outline" onClick={() => void sides.refetch()}>{t("protectedSides.retry")}</Button> : null }
			<ConfirmDialog open={closeId !== undefined} title={t("protectedSides.close")} description={t("protectedSides.confirm")}
				confirmLabel={t("protectedSides.close")} destructive busy={close.isPending} error={close.error?.message}
				onOpenChange={(open) => { if (!open) setCloseId(undefined); }} onConfirm={() => { if (closeId) close.mutate(closeId); }} />
		</SettingsSection>
	);
}
