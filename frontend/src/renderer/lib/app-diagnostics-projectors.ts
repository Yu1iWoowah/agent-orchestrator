import type { CommittedObservation } from "./app-diagnostics-registry";
import { diagnosticId, type SessionObservation, type WorkspaceObservation } from "../../shared/app-diagnostics";

function record(value: unknown): Record<string, unknown> {
	return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}
export function safeId(value: unknown): string {
	return typeof value === "string" && diagnosticId(value) ? value : "";
}
export function safeEnum(value: unknown, allowed: string, fallback = "unknown"): string {
	return typeof value === "string" && allowed.split(",").includes(value) ? value : fallback;
}
export function projectSession(value: unknown): SessionObservation | undefined {
	const row = record(value);
	const id = safeId(row.id);
	if (!id) return undefined;
	return {
		id,
		kind: safeEnum(row.kind, "worker,orchestrator"),
		terminated: row.isTerminated === true,
		status: safeEnum(
			row.status,
			"spawning,working,pr_open,draft,ci_failed,review_pending,changes_requested,commented,approved,mergeable,ready_to_merge,merged,cleanup,done,exited,killed,stuck,needs_input,no_signal,idle,terminated",
		),
		activity: safeEnum(record(row.activity).state, "active,idle,waiting_input,blocked,exited"),
		column: safeEnum(row.kanbanColumn, "building,validating,needs_review,needs_input,ready,done,archive"),
	};
}
export function projectWorkspaces(value: unknown, scope?: ReadonlySet<string>): WorkspaceObservation[] {
	if (!Array.isArray(value)) return [];
	const out: WorkspaceObservation[] = [];
	const prioritized = scope
		? [...value.filter((row) => scope.has(safeId(record(row).id))), ...value.filter((row) => !scope.has(safeId(record(row).id)))]
		: value;
	for (const raw of prioritized) {
		if (out.length >= 100) break;
		const row = record(raw);
		const projectId = safeId(row.id);
		if (!projectId) continue;
		const sessions = Array.isArray(row.sessions) ? row.sessions : [];
		const hostId = safeId(row.hostId);
		out.push({
			scope: !scope || scope.has(projectId) ? "ids" : "counts",
			projectId,
			hostId,
			source: hostId ? "remote" : row.kind === "cloud" ? "cloud" : "local",
			shared: Boolean(row.shared),
			folderMissing: row.folderMissing === true,
			sessionCount: Math.min(1e7, sessions.length),
			sessions:
				!scope || scope.has(projectId)
					? sessions.slice(0, 200).flatMap((s) => {
							const item = projectSession(s);
							return item ? [item] : [];
						})
					: [],
			omittedCount: !scope || scope.has(projectId) ? Math.max(0, sessions.length - 200) : 0,
		});
	}
	return out;
}
export function sessionIds(value: readonly unknown[]): string[] {
	return value.slice(0, 200).flatMap((item) => {
		const id = safeId(record(item).id);
		return id ? [id] : [];
	});
}
export function componentObservation(
	kind: CommittedObservation["kind"],
	workspace: unknown,
	options: {
		expanded?: boolean;
		shared?: boolean;
		showAll?: boolean;
		hiddenCount?: number;
		sessionCount?: number;
		live?: readonly unknown[];
		workers?: readonly unknown[];
		ordered?: readonly unknown[];
		listed?: readonly unknown[];
		workspaces?: unknown;
	} = {},
): CommittedObservation {
	const row = record(workspace);
	const hostId = safeId(row.hostId);
	return {
		kind,
		omittedCount: Math.min(
			1e7,
			[options.live, options.workers, options.ordered, options.listed].reduce(
				(sum, rows) => sum + Math.max(0, (rows?.length ?? 0) - 200),
				0,
			) + Math.max(0, (Array.isArray(options.workspaces) ? options.workspaces.length : 0) - 100),
		),
		projectId: safeId(row.id),
		hostId,
		source: hostId ? "remote" : row.kind === "cloud" ? "cloud" : "local",
		expanded: options.expanded === true,
		shared: options.shared === true,
		showAll: options.showAll === true,
		sessionCount: Math.min(1e7, options.sessionCount ?? (Array.isArray(row.sessions) ? row.sessions.length : 0)),
		hiddenCount: Math.min(1e7, Math.max(0, options.hiddenCount ?? 0)),
		liveIds: sessionIds(options.live ?? []),
		workerIds: sessionIds(options.workers ?? []),
		orderedIds: sessionIds(options.ordered ?? []),
		listedIds: sessionIds(options.listed ?? []),
		workspaces: projectWorkspaces(options.workspaces ?? []),
	};
}
