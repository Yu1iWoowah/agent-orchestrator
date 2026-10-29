import { useState } from "react";
import { useTranslation } from "react-i18next";
import {
	AttentionUnavailable,
	type AttentionItem,
	useSessionAttention,
} from "../../hooks/useSessionAttention";
import { Button } from "../ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import {
	useConversationFrameNavigation,
	type ConversationFrameDescriptor,
} from "./ConversationFrames";

export function useSessionAttentionFrame({
	sessionId,
	hostId,
	branchId,
	onLocate,
	locatingId,
}: {
	sessionId: string;
	hostId?: string;
	branchId?: string;
	onLocate: (id: string) => void;
	locatingId?: string;
}): ConversationFrameDescriptor {
	const { t } = useTranslation();
	const [tab, setTab] = useState("open");
	const open = useSessionAttention(sessionId, hostId, branchId, "open");
	const history = useSessionAttention(
		sessionId,
		hostId,
		branchId,
		"history",
		tab === "history",
	);
	const query = tab === "history" ? history : open;
	const items = [
		...new Map(
			query.data?.pages
				.flatMap((page) => page.items)
				.map((item) => [item.activity.id, item]),
		).values(),
	];
	const summary = open.isError
		? "?"
		: open.isPending
			? "…"
			: String(open.data?.pages[0]?.counts.open ?? 0);
	return {
		kind: "attention",
		title: t("attention.title"),
		summary,
		content: (
			<Tabs value={tab} onValueChange={setTab}>
				<TabsList className="mb-3 h-8">
					<TabsTrigger value="open">{t("attention.pending")}</TabsTrigger>
					<TabsTrigger value="history">{t("attention.history")}</TabsTrigger>
				</TabsList>
				{(["open", "history"] as const).map((value) => (
					<TabsContent key={value} value={value}>
						{query.isError ? (
							<div role="alert" className="text-xs text-muted-foreground">
								<p>
									{query.error instanceof AttentionUnavailable
										? t("attention.unsupported")
										: t("attention.offline")}
								</p>
								{query.error instanceof AttentionUnavailable ? null : (
									<Button
										variant="ghost"
										size="sm"
										aria-label={t("attention.retry")}
										onClick={() => void query.refetch()}
									>
										{t("attention.retry")}
									</Button>
								)}
							</div>
						) : query.isPending ? (
							<p role="status" className="text-xs text-muted-foreground">
								{t("attention.loading")}
							</p>
						) : items.length === 0 ? (
							<p className="text-xs text-muted-foreground">
								{t(
									value === "open" ? "attention.empty" : "attention.noHistory",
								)}
							</p>
						) : (
							<ul className="divide-y divide-border">
								{items.map((item) => (
									<AttentionRow
										key={item.activity.id}
										item={item}
										onLocate={onLocate}
										locating={locatingId === item.activity.id}
									/>
								))}
							</ul>
						)}
						{query.hasNextPage ? (
							<Button
								variant="ghost"
								size="sm"
								disabled={query.isFetchingNextPage}
								onClick={() => void query.fetchNextPage()}
							>
								{t("attention.older")}
							</Button>
						) : null}
					</TabsContent>
				))}
			</Tabs>
		),
	};
}

function AttentionRow({
	item,
	onLocate,
	locating,
}: {
	item: AttentionItem;
	onLocate: (id: string) => void;
	locating: boolean;
}) {
	const { t } = useTranslation();
	const navigate = useConversationFrameNavigation();
	const message = item.activity.detail?.message;
	const title =
		typeof message === "string" && message.trim()
			? message
			: item.activity.summary || t("attention.request");
	const status =
		item.activity.status === "resolved" || item.activity.status === "completed"
			? "attention.answered"
			: item.activity.status === "cancelled"
				? "attention.cancelled"
				: "attention.unavailable";
	return (
		<li className="min-w-0 py-3 first:pt-0">
			<div className="mb-1 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
				<span>
					{t(
						item.activity.activityKind === "approval"
							? "attention.approval"
							: "attention.input",
					)}
				</span>
				<span>{t("attention.thisSession")}</span>
				{item.actionable && !item.read ? (
					<span className="text-status-needs-you">{t("attention.unread")}</span>
				) : null}
			</div>
			<p className="whitespace-pre-wrap break-words text-sm">{title}</p>
			{item.actionable ? (
				<Button
					variant="ghost"
					size="sm"
					className="mt-2 max-w-full"
					aria-label={t("attention.goTo", { title })}
					disabled={locating}
					onClick={() =>
						navigate
							? navigate(() => onLocate(item.activity.id))
							: onLocate(item.activity.id)
					}
				>
					{locating ? t("attention.locating") : t("attention.original")}
				</Button>
			) : (
				<p className="mt-2 text-xs text-muted-foreground">{t(status)}</p>
			)}
		</li>
	);
}
