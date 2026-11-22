import { useTranslation } from "react-i18next";
import type { components } from "../../../api/schema";
import { Button } from "../ui/button";

export function SideCompactionNotice({ protection, onClose, busy = false }: {
	protection?: components["schemas"]["SideProtection"];
	onClose: () => void;
	busy?: boolean;
}) {
	const { t } = useTranslation();
	if (!protection) return null;
	return (
		<section aria-label={t("protectedSides.title")} className="space-y-2 text-sm">
			<p className="font-medium">{t(protection.result === "pending" ? "protectedSides.pending" : "protectedSides.unknown")}</p>
			{protection.closed ? <p>{t("protectedSides.closed")}</p> : (
				<>
					<p>{t("protectedSides.isolated")}</p>
					<p>{t(protection.shutdownBlocked ? "protectedSides.blocked" : "protectedSides.allowed")}</p>
					<p className="text-muted-foreground">{t("protectedSides.reason", { reason: t(`protectedSides.reasons.${protection.reason}`, { defaultValue: protection.reason }) })}</p>
					<Button size="sm" variant="outline" disabled={busy} onClick={onClose}>{t("protectedSides.close")}</Button>
				</>
			)}
			<details className="text-muted-foreground">
				<summary className="cursor-pointer">{t("protectedSides.risks")}</summary>
				<ul className="mt-2 list-disc space-y-1 pl-5">
					<li>{t("protectedSides.external")}</li>
					<li>{t("protectedSides.descendants")}</li>
					<li>{t("protectedSides.writable")}</li>
					<li>{t("protectedSides.platform")}</li>
				</ul>
			</details>
		</section>
	);
}
