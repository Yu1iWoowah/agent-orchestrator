import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { apiClient } from "../lib/api-client";
import { SettingsSection } from "./settings/SettingsSection";

type Composition = components["schemas"]["UsageCompositionResponse"];
type Category = components["schemas"]["UsageCompositionCategory"];

const number = (value: number | null | undefined) => (typeof value === "number" ? value.toLocaleString() : "–");

// Shows each model request's exact input and its split by context category,
// exactly as the daemon computed it at query time.
export function SessionCompositionPanel({ sessionId }: { sessionId: string }) {
	const { t } = useTranslation();
	const query = useQuery({
		queryKey: ["session-composition", sessionId],
		queryFn: async (): Promise<Composition> => {
			const { data, error } = await apiClient.GET("/api/v1/usage/sessions/{sessionId}/composition", {
				params: { path: { sessionId } },
			});
			if (error || !data) throw error ?? new Error("composition unavailable");
			return data;
		},
		retry: 1,
	});
	const data = query.data;
	const header = data?.requests[0]?.categories ?? [];
	const notes = [...new Map(header.filter((cat) => cat.note).map((cat) => [cat.category, cat] as const)).values()];
	return (
		<SettingsSection title={t("composition.title")} grouped testId="session-composition">
			<div className="settings-content-row space-y-3 p-4 text-xs text-muted-foreground" aria-busy={query.isPending}>
				{query.isPending ? <p>{t("composition.loading")}</p> : null}
				{query.isError ? <p role="alert">{t("composition.error")}</p> : null}
				{data && data.status !== "ok" ? <p>{t(`composition.reason.${data.reason || "no_source"}`)}</p> : null}
				{data?.status === "ok" ? (
					<>
						<p>{t("composition.method")}</p>
						<div className="overflow-x-auto">
							<table className="w-full text-left text-xs" aria-label={t("composition.title")}>
								<thead>
									<tr className="border-b border-border">
										<th className="py-2 pr-2 font-medium">{t("composition.request")}</th>
										<th className="px-2 text-right font-medium">{t("composition.input")}</th>
										{header.map((cat) => (
											<th className="px-2 text-right font-medium" key={cat.category}>
												{t(`composition.category.${cat.category}`)}
											</th>
										))}
									</tr>
								</thead>
								<tbody>
									{data.requests.map((req) => (
										<tr className="border-b border-border last:border-0" key={req.ordinal}>
											<th className="py-2 pr-2 font-normal tabular-nums">{req.ordinal}</th>
											<td className="px-2 text-right tabular-nums text-foreground">
												{number(req.inputTokens)}
												{req.unresolved ? <div>{t("composition.unresolved", { value: req.unresolved })}</div> : null}
											</td>
											{req.categories.map((cat) => (
												<CategoryCell cat={cat} key={cat.category} />
											))}
										</tr>
									))}
								</tbody>
							</table>
						</div>
						{notes.length > 0 ? (
							<ul className="space-y-1">
								{notes.map((cat) => (
									<li key={cat.category}>
										{t(`composition.category.${cat.category}`)}: {t(`composition.note.${cat.note || "unknown"}`)}
									</li>
								))}
							</ul>
						) : null}
					</>
				) : null}
			</div>
		</SettingsSection>
	);
}

function CategoryCell({ cat }: { cat: Category }) {
	const { t } = useTranslation();
	return (
		<td className="px-2 text-right tabular-nums">
			<div className="text-foreground">{number(cat.tokens)}</div>
			<div>{cat.bytes == null ? t("composition.note.unknown") : t("composition.bytes", { value: number(cat.bytes) })}</div>
		</td>
	);
}
