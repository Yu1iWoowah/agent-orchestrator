import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { MessageCircleQuestion } from "lucide-react";
import {
	useProjectAttention,
	projectAttentionQueryKey,
	type AttentionAudience,
	type ProjectAttentionItem,
} from "../hooks/useProjectAttention";
import { clientForSessionHost } from "../lib/host-clients";
import { sessionNavigateTarget } from "../lib/navigate-to-session";
import type { DecisionTarget } from "../lib/decision-target";
import type { FrameRect } from "../lib/conversation-frames";
import { DecisionDialog } from "./chat/DecisionDialog";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogHeader,
	DialogTitle,
	DialogTrigger,
} from "./ui/dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "./ui/tabs";
import { Button } from "./ui/button";
import { TopbarButton } from "./TopbarButton";

function decisionTarget(
	item: ProjectAttentionItem,
): DecisionTarget | undefined {
	switch (item.kind) {
		case "agent_question":
			return item.questionId
				? { kind: item.kind, questionId: item.questionId, title: item.title }
				: undefined;
		case "worker_report":
			return item.reportId
				? {
						kind: item.kind,
						reportId: item.reportId,
						originSessionId: item.sessionId,
						originHarness: item.originHarness ?? "unknown",
						title: item.title,
						explanation: item.title,
					}
				: undefined;
		case "provider_approval":
		case "provider_input":
			return item.activityId &&
				item.requestId &&
				item.conversationId &&
				item.branchId &&
				item.sourceBranchId
				? {
						kind: item.kind,
						activityId: item.activityId,
						requestId: item.requestId,
						conversationId: item.conversationId,
						branchId: item.branchId,
						sourceBranchId: item.sourceBranchId,
						title: item.title,
					}
				: undefined;
	}
}

/** The sidebar count is read-only and never interrupts the current editor or Browser. */
export function ProjectAttentionCount({
	projectId,
	hostId,
}: {
	projectId: string;
	hostId?: string;
}) {
	const { t } = useTranslation();
	const query = useProjectAttention(projectId, hostId, "user");
	const count = query.isError ? "?" : query.data?.pages[0]?.open;
	if (count === 0 || count === undefined) return null;
	return (
		<span
			className="sidebar-expanded-chrome relative z-[1] shrink-0 text-xs text-status-needs-you group-data-[collapsible=icon]:hidden"
			aria-label={t("projectAttention.count", { count: String(count) })}
			title={t("projectAttention.count", { count: String(count) })}
		>
			{count}
		</span>
	);
}

export function ProjectAttention({
	projectId,
	hostId,
}: {
	projectId: string;
	hostId?: string;
}) {
	const { t } = useTranslation();
	const client = useQueryClient();
	const navigate = useNavigate();
	const [open, setOpen] = useState(false);
	const [audience, setAudience] = useState<AttentionAudience>("user");
	const [selected, setSelected] = useState<{
		item: ProjectAttentionItem;
		target: DecisionTarget;
	}>();
	const [bounds, setBounds] = useState<{
		surface: HTMLElement;
		rect: FrameRect;
	}>();
	const [readError, setReadError] = useState(false);
	const rows = useRef(new Map<string, HTMLButtonElement>());
	const returnRows = useRef<string[]>([]);
	const removedFocus = useRef<string | undefined>(undefined);
	const listTitle = useRef<HTMLHeadingElement>(null);
	const listContent = useRef<HTMLDivElement>(null);
	const scrollPositions = useRef(new Map<AttentionAudience, number>());
	const human = useProjectAttention(projectId, hostId, "user");
	const coordination = useProjectAttention(
		projectId,
		hostId,
		"orchestrator",
		open && audience === "orchestrator",
	);
	const query = audience === "user" ? human : coordination;
	const count = human.isError ? "?" : (human.data?.pages[0]?.open ?? "…");
	const items = new Map(
		(query.data?.pages ?? [])
			.flatMap((page) => page.items)
			.map((item) => [item.id, item]),
	);
	useEffect(() => {
		if (!selected) return;
		const surface =
			document.querySelector<HTMLElement>(".center-panel-surface") ??
			document.documentElement;
		const measure = () => {
			const box = surface.getBoundingClientRect();
			setBounds({
				surface,
				rect: {
					left: 16,
					top: 16,
					width: Math.max(0, box.width - 32),
					height: Math.max(0, box.height - 32),
				},
			});
		};
		measure();
		const observer = new ResizeObserver(measure);
		observer.observe(surface);
		window.addEventListener("resize", measure);
		return () => {
			observer.disconnect();
			window.removeEventListener("resize", measure);
		};
	}, [selected]);
	useEffect(() => {
		if (
			!selected ||
			selected.item.kind !== "worker_report" ||
			!selected.item.reportId
		)
			return;
		let disposed = false;
		setReadError(false);
		void Promise.resolve()
			.then(() =>
				clientForSessionHost(hostId).POST(
					"/api/v1/projects/{id}/attention/reports/{reportId}/read",
					{
						params: {
							path: { id: projectId, reportId: selected.item.reportId ?? "" },
						},
					},
				),
			)
			.then(({ error }) => {
				if (disposed) return;
				setReadError(Boolean(error));
				if (!error)
					void client.invalidateQueries(
						{ queryKey: projectAttentionQueryKey(projectId, hostId) },
						{ cancelRefetch: false },
					);
			})
			.catch(() => {
				if (!disposed) setReadError(true);
			});
		return () => {
			disposed = true;
		};
	}, [selected, projectId, hostId, client]);
	useEffect(() => {
		const panel = listContent.current?.querySelector<HTMLElement>(
			'[role="tabpanel"][data-state="active"]',
		);
		if (panel) panel.scrollTop = scrollPositions.current.get(audience) ?? 0;
	}, [audience]);
	const goSession = (item: ProjectAttentionItem) => {
		setSelected(undefined);
		setOpen(false);
		void navigate(sessionNavigateTarget(projectId, item.sessionId, hostId));
	};
	const restoreListFocus = () => {
		requestAnimationFrame(() => {
			const panel = listContent.current?.querySelector<HTMLElement>(
				'[role="tabpanel"][data-state="active"]',
			);
			if (panel) panel.scrollTop = scrollPositions.current.get(audience) ?? 0;
			const target = returnRows.current
				.map((id) => rows.current.get(id))
				.find((row) => row?.isConnected);
			const fallback = returnRows.current.length ? panel : undefined;
			(target ?? fallback ?? listTitle.current)?.focus({ preventScroll: true });
		});
	};
	useEffect(() => {
		if (open && !selected && removedFocus.current && !rows.current.has(removedFocus.current)) {
			restoreListFocus();
		}
		removedFocus.current = undefined;
	});
	if (human.data?.pages[0]?.unsupported) return null;
	return (
		<>
			{!selected ? (
				<Dialog open={open} onOpenChange={setOpen}>
					<DialogTrigger asChild>
						<TopbarButton
							variant="secondary"
							aria-label={t("projectAttention.open")}
						>
							<MessageCircleQuestion className="size-4" />
							<span>
								{t("attention.title")} {count}
							</span>
						</TopbarButton>
					</DialogTrigger>
					<DialogContent
						ref={listContent}
						className="max-h-[calc(100dvh-32px)] max-w-[560px] overflow-hidden"
						onOpenAutoFocus={(event) => {
							event.preventDefault();
							restoreListFocus();
						}}
					>
						<DialogHeader>
							<DialogTitle ref={listTitle} tabIndex={-1}>
								{t("projectAttention.title")}
							</DialogTitle>
							<DialogDescription>
								{t("projectAttention.description")}
							</DialogDescription>
						</DialogHeader>
						<Tabs
							className="flex min-h-0 flex-col"
							value={audience}
							onValueChange={(value) => {
								if (value === "user" || value === "orchestrator")
									setAudience(value);
							}}
						>
							<TabsList className="shrink-0 justify-start">
								<TabsTrigger value="user">
									{t("attention.title")} {count}
								</TabsTrigger>
								<TabsTrigger value="orchestrator">
									{t("projectAttention.coordination")}
								</TabsTrigger>
							</TabsList>
							{(["user", "orchestrator"] as const).map((tab) => (
								<TabsContent
									key={tab}
									value={tab}
									className="min-h-0 overflow-y-auto"
									onScroll={(event) =>
										scrollPositions.current.set(
											tab,
											event.currentTarget.scrollTop,
										)
									}
								>
									<p
										className="mb-3 text-xs text-muted-foreground"
										role="status"
									>
										{t("projectAttention.totals", {
											open: query.isError
												? "?"
												: (query.data?.pages[0]?.open ?? "…"),
											unread: query.isError
												? "?"
												: (query.data?.pages[0]?.unread ?? "…"),
										})}
									</p>
									{query.isPending ? (
										<p role="status">{t("attention.loading")}</p>
									) : null}
									{query.isError ? (
										<div role="alert">
											<p>{t("projectAttention.offline")}</p>
											<Button
												variant="ghost"
												size="sm"
												onClick={() => void query.refetch()}
											>
												{t("attention.retry")}
											</Button>
										</div>
									) : null}
									{query.isSuccess && !items.size ? (
										<p className="text-sm text-muted-foreground">
											{t("attention.empty")}
										</p>
									) : null}
									<ul className="divide-y divide-border">
										{[...items.values()].map((item) => (
											<li key={item.id} className="space-y-2 py-3">
												<button
													type="button"
													className="w-full rounded-sm text-left outline-none focus-visible:ring-2 focus-visible:ring-ring"
													ref={(element) => {
														if (element) rows.current.set(item.id, element);
														else {
															if (rows.current.get(item.id) === document.activeElement) removedFocus.current = item.id;
															rows.current.delete(item.id);
														}
													}}
													onClick={() => {
														const target = decisionTarget(item);
														if (target) {
															const ids = [...items.keys()];
															const index = ids.indexOf(item.id);
															returnRows.current = [...ids.slice(index), ...ids.slice(0, index).reverse()];
															setSelected({ item, target });
														}
													}}
												>
													<span className="block text-xs text-muted-foreground">
														{item.sessionName || item.sessionId} ·{" "}
														{item.sessionId}
														{!item.read ? (
															<span className="ml-2 text-status-needs-you">
																{t("attention.unread")}
															</span>
														) : null}
													</span>
													<span className="block whitespace-pre-wrap break-words text-sm">
														{item.title}
													</span>
												</button>
												{item.kind === "worker_report" ? (
													<p className="text-xs text-muted-foreground">
														{t("projectAttention.reportStatus", {
															delivery: item.deliveryState,
														})}
													</p>
												) : null}
												<a
													className="text-xs text-muted-foreground underline underline-offset-4"
													href={`ao://sessions/${encodeURIComponent(projectId)}/${encodeURIComponent(item.sessionId)}`}
													onClick={(event) => {
														event.preventDefault();
														goSession(item);
													}}
												>
													{t("decision.original")}
												</a>
											</li>
										))}
									</ul>
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
					</DialogContent>
				</Dialog>
			) : null}
			{selected && bounds ? (
				<DecisionDialog
					target={selected.target}
					sessionId={selected.item.sessionId}
					hostId={hostId}
					bounds={bounds}
					onClose={() => setSelected(undefined)}
					onLocate={() => goSession(selected.item)}
					onRestoreFocus={restoreListFocus}
				/>
			) : null}
			{readError ? (
				<p role="alert" className="text-xs text-muted-foreground">
					{t("decision.readError")}
				</p>
			) : null}
		</>
	);
}
