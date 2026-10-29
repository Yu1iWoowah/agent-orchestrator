import type { QueryClient } from "@tanstack/react-query";
import {
	DIAGNOSTIC_MAX_BYTES,
	isCollector,
	type Collector,
	type GeometryNode,
	type GeometryStyle,
	type QueryObservation,
} from "../../shared/app-diagnostics";
import { diagnosticCommitRevision, diagnosticRegions, diagnosticsEnabled, setDiagnosticRecording } from "./app-diagnostics-registry";
import { projectWorkspaces, safeEnum, safeId } from "./app-diagnostics-projectors";

export type RouteObservation = Pick<Collector["route"], "routeId" | "source" | "projectId" | "sessionId" | "hostId" | "revision">;
const bounded = (v: number) => (Number.isFinite(v) ? Math.max(-1e6, Math.min(1e6, v)) : 0);
function styleOf(element: HTMLElement): GeometryStyle {
	const rect = element.getBoundingClientRect();
	const css = getComputedStyle(element);
	const numbers = (value: string, max: number) => (value.match(/-?\d+(?:\.\d+)?/g) ?? []).slice(0, max).map((v) => bounded(Number(v)));
	return {
		box: { x: bounded(rect.x), y: bounded(rect.y), width: Math.max(0, bounded(rect.width)), height: Math.max(0, bounded(rect.height)) },
		display: safeEnum(css.display, "none,block,flex,grid,inline,inline-block,inline-flex,inline-grid,contents,table", "other"),
		visibility: safeEnum(css.visibility, "visible,hidden,collapse", "other"),
		opacity: css.opacity === "" ? 1 : Math.max(0, Math.min(1, bounded(Number(css.opacity)))),
		overflowX: safeEnum(css.overflowX, "visible,hidden,clip,auto,scroll", "other"),
		overflowY: safeEnum(css.overflowY, "visible,hidden,clip,auto,scroll", "other"),
		clip: css.clipPath === "none" || !css.clipPath ? "none" : /^inset\([-\d.%\spx]+\)$/.test(css.clipPath) ? "inset" : "other",
		gridRows: /^[-\d.px\s]+$/.test(css.gridTemplateRows) ? numbers(css.gridTemplateRows, 64) : [],
		transform: /^matrix(?:3d)?\([-\d.,\s]+\)$/.test(css.transform) ? numbers(css.transform.slice(css.transform.indexOf("(")), 16) : [],
		zIndex: bounded(Number(css.zIndex)),
	};
}
function geometryNode(
	role: string,
	element: HTMLElement | null,
	projectId: string,
	sessionId: string,
	source: string,
	hostId: string,
	id: number,
	roots: readonly HTMLElement[],
): GeometryNode {
	const ancestors: GeometryStyle[] = [];
	if (!element?.isConnected)
		return {
			id: `node-${id}`,
			source,
			hostId,
			role,
			projectId,
			sessionId,
			availability: "not_mounted",
			ancestors,
			intersectsViewport: false,
			hit: "not_tested",
		};
	const style = styleOf(element);
	let parent = element.parentElement;
	for (let i = 0; parent && i < 8; i++, parent = parent.parentElement) ancestors.push(styleOf(parent));
	const box = style.box;
	const intersectsViewport =
		box.width > 0 && box.height > 0 && box.x < innerWidth && box.y < innerHeight && box.x + box.width > 0 && box.y + box.height > 0;
	let hit = "not_tested";
	if (intersectsViewport && typeof document.elementFromPoint === "function") {
		const target = document.elementFromPoint(
			Math.max(0, Math.min(innerWidth - 1, box.x + box.width / 2)),
			Math.max(0, Math.min(innerHeight - 1, box.y + box.height / 2)),
		);
		hit = !target
			? "not_tested"
			: element.contains(target)
				? "clear"
				: roots.some((root) => root.contains(target))
					? "registered"
					: "unregistered_overlay";
	}
	return {
		id: `node-${id}`,
		source,
		hostId,
		role,
		projectId,
		sessionId,
		availability: "available",
		style,
		ancestors,
		intersectsViewport,
		hit,
	};
}
function queryKind(key: readonly unknown[]): { kind: string; hostId: string } | undefined {
	if (
		key.length === 1 &&
		typeof key[0] === "string" &&
		["workspaces", "cloud-projects", "cloud-sessions", "cloud-shared-workspaces"].includes(key[0])
	)
		return { kind: key[0], hostId: "" };
	if (key[0] !== "remote-workspaces" || !safeId(key[1])) return;
	if (key.length === 2) return { kind: "remote-workspaces", hostId: safeId(key[1]) };
	if (key.length === 3 && (key[2] === "projects" || key[2] === "sessions"))
		return { kind: key[2] === "projects" ? "remote-projects" : "remote-sessions", hostId: safeId(key[1]) };
	return undefined;
}
export function createAppDiagnosticsCollector(options: {
	queryClient: QueryClient;
	readRoute: () => RouteObservation;
	observeRoute?: () => () => void;
	readInspectorOpen?: (route: RouteObservation) => boolean;
}) {
	let enabledAt = "";
	let cacheRevision = 0;
	let errorsRevision = 0;
	let dropped = 0;
	let stopCache: (() => void) | undefined;
	let stopRoute: (() => void) | undefined;
	let restoreConsole: (() => void) | undefined;
	const errors: Collector["errors"]["records"] = [];
	const addError = (source: string, value: unknown) => {
		if (!enabledAt) return;
		let name = "Unknown";
		try {
			if (value instanceof Error) {
				const ownName = Object.getOwnPropertyDescriptor(value, "name");
				const inheritedName = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(value), "name");
				const descriptor = ownName ?? inheritedName;
				if (descriptor && "value" in descriptor)
					name = safeEnum(descriptor.value, "Error,TypeError,RangeError,ReferenceError,SyntaxError", "Unknown");
			}
		} catch {
			/* An opaque error still receives the fixed Unknown summary. */
		}
		const last = errors.at(-1);
		if (last?.source === source && last.name === name) last.count = Math.min(1e7, last.count + 1);
		else {
			if (errors.length >= 100) {
				errors.shift();
				dropped = Math.min(1e7, dropped + 1);
			}
			errors.push({ at: new Date().toISOString(), source, name, message: "[redacted]", count: 1 });
		}
		errorsRevision++;
	};
	const onError = (event: ErrorEvent) => addError("window_error", event.error);
	const onRejection = (event: PromiseRejectionEvent) => addError("unhandled_rejection", event.reason);
	const disable = () => {
		stopRoute?.();
		stopRoute = undefined;
		stopCache?.();
		stopCache = undefined;
		restoreConsole?.();
		restoreConsole = undefined;
		window.removeEventListener("error", onError);
		window.removeEventListener("unhandledrejection", onRejection);
		errors.length = 0;
		enabledAt = "";
		dropped = 0;
		setDiagnosticRecording(false);
	};
	const enable = () => {
		if (enabledAt) return;
		stopRoute = options.observeRoute?.();
		enabledAt = new Date().toISOString();
		cacheRevision = 1;
		errorsRevision = 1;
		stopCache = options.queryClient.getQueryCache().subscribe(() => {
			cacheRevision++;
		});
		const original = console.error;
		let recording = true;
		const wrapped: typeof console.error = (...args) => {
			if (recording && enabledAt) addError("console_error", args[0]);
			original.apply(console, args);
		};
		console.error = wrapped;
		restoreConsole = () => {
			recording = false;
			if (console.error === wrapped) console.error = original;
		};
		window.addEventListener("error", onError);
		window.addEventListener("unhandledrejection", onRejection);
		setDiagnosticRecording(true);
	};
	const capture = (projectId: string): Collector => {
		if (!enabledAt || !diagnosticsEnabled()) throw new Error("APP_DIAGNOSTICS_DISABLED");
		const started = new Date().toISOString();
		const route = options.readRoute();
		const cacheStart = cacheRevision;
		const commitStart = diagnosticCommitRevision();
		const scope = new Set([safeId(projectId), safeId(route.projectId)].filter(Boolean));
		const provenance = (availability: string, revision: number, omittedCount = 0) => ({
			availability,
			revision,
			omittedCount,
			observedAt: started,
		});
		const queries: QueryObservation[] = [];
		let omittedQueries = 0;
		for (const query of options.queryClient.getQueryCache().getAll()) {
			const key = queryKind(query.queryKey);
			if (!key) continue;
			if (queries.length >= 100) {
				omittedQueries++;
				continue;
			}
			queries.push({
				...key,
				status: query.state.status,
				fetchStatus: query.state.fetchStatus,
				invalidated: query.state.isInvalidated,
				dataUpdatedAt: query.state.dataUpdatedAt,
				errorUpdatedAt: query.state.errorUpdatedAt,
				observerCount: query.getObserversCount(),
				failureCount: query.state.fetchFailureCount,
				count: Array.isArray(query.state.data) ? Math.min(1e7, query.state.data.length) : 0,
				workspaces: key.kind === "workspaces" ? projectWorkspaces(query.state.data, scope) : [],
			});
		}
		const committed = diagnosticRegions();
		const entries = committed.regions.map(({ value }) => ({
			...value,
			omittedCount: value.projectId && !scope.has(value.projectId) ? 0 : value.omittedCount,
			liveIds: value.projectId && !scope.has(value.projectId) ? [] : [...value.liveIds],
			workerIds: value.projectId && !scope.has(value.projectId) ? [] : [...value.workerIds],
			orderedIds: value.projectId && !scope.has(value.projectId) ? [] : [...value.orderedIds],
			listedIds: value.projectId && !scope.has(value.projectId) ? [] : [...value.listedIds],
			workspaces: value.workspaces.map((row) => ({
				...row,
				omittedCount: scope.has(row.projectId) ? row.omittedCount : 0,
				scope: scope.has(row.projectId) ? "ids" : "counts",
				sessions: scope.has(row.projectId) ? row.sessions.map((session) => ({ ...session })) : [],
			})),
		}));
		const roots = committed.regions.flatMap((r) => {
			const node = r.root();
			return node ? [node] : [];
		});
		const nodes: GeometryNode[] = [];
		let omittedNodes = 0;
		let ancestorTruncatedCount = 0;
		const addNode = (role: string, node: HTMLElement | null, project: string, source: string, hostId: string, session = "") => {
			if (nodes.length >= 1000) {
				omittedNodes++;
				return;
			}
			let ancestor = node?.parentElement;
			for (let i = 0; ancestor && i < 8; i++) ancestor = ancestor.parentElement;
			if (ancestor) ancestorTruncatedCount++;
			nodes.push(geometryNode(role, node, project, session, source, hostId, nodes.length + 1, roots));
		};
		for (const region of committed.regions) {
			const { value } = region;
			addNode(
				value.kind === "project" ? "project_root" : value.kind === "sidebar" ? "sidebar" : "main_pane",
				region.root(),
				value.projectId,
				value.source,
				value.hostId,
			);
			if (value.kind !== "project" || !scope.has(value.projectId)) continue;
			const list = region.list?.() ?? null;
			addNode("session_list", list, value.projectId, value.source, value.hostId);
			const rows = list?.querySelectorAll<HTMLElement>("[data-session-row]");
			value.listedIds.forEach((id, index) =>
				addNode("session_row", rows?.item(index) ?? null, value.projectId, value.source, value.hostId, id),
			);
		}
		const sidebar = committed.regions.find((entry) => entry.value.kind === "sidebar");
		const value: Collector = {
			redactionSchemaVersion: 1,
			captureStartedAt: started,
			captureFinishedAt: new Date().toISOString(),
			consistency: "stable",
			route: { ...provenance(route.routeId === "unsupported_surface" ? "unsupported_surface" : "available", route.revision), ...route },
			shell: {
				...provenance(sidebar ? "available" : "unavailable", commitStart),
				width: Math.max(0, bounded(innerWidth)),
				height: Math.max(0, bounded(innerHeight)),
				devicePixelRatio: Math.max(0.1, Math.min(16, devicePixelRatio || 1)),
				sidebarCollapsed: sidebar ? !sidebar.value.expanded : false,
				sidebarWidth: Math.max(0, bounded(sidebar?.root()?.getBoundingClientRect().width ?? 0)),
				inspectorOpen: options.readInspectorOpen?.(route) ?? false,
			},
			cache: {
				...provenance(
					queries.length ? "available" : "unavailable",
					cacheStart,
					omittedQueries +
						queries.reduce(
							(sum, q) =>
								sum +
								(q.kind === "workspaces"
									? Math.max(0, q.count - q.workspaces.length) + q.workspaces.reduce((count, row) => count + row.omittedCount, 0)
									: 0),
							0,
						),
				),
				queries,
			},
			components: {
				...provenance(
					entries.length ? "available" : "not_mounted",
					commitStart,
					committed.omitted +
						entries.reduce(
							(sum, entry) => sum + entry.omittedCount + entry.workspaces.reduce((count, row) => count + row.omittedCount, 0),
							0,
						),
				),
				entries,
			},
			geometry: { ...provenance(nodes.length ? "available" : "not_mounted", commitStart, omittedNodes), ancestorTruncatedCount, nodes },
			errors: {
				...provenance("available", errorsRevision, dropped),
				enabledAt,
				notRecordedBeforeEnable: true,
				records: errors.map((row) => ({ ...row })),
			},
		};
		const finishedRoute = options.readRoute();
		if (
			JSON.stringify(route) !== JSON.stringify(finishedRoute) ||
			cacheRevision !== cacheStart ||
			diagnosticCommitRevision() !== commitStart
		)
			value.consistency = "changed";
		while (new TextEncoder().encode(JSON.stringify(value)).length > DIAGNOSTIC_MAX_BYTES - 4096) {
			if (value.geometry.nodes.length) {
				value.geometry.nodes.pop();
				value.geometry.omittedCount++;
			} else if (value.components.entries.length) {
				value.components.entries.pop();
				value.components.omittedCount++;
			} else if (value.cache.queries.length) {
				value.cache.queries.pop();
				value.cache.omittedCount++;
			} else throw new Error("APP_DIAGNOSTICS_PAYLOAD_TOO_LARGE");
		}
		value.captureFinishedAt = new Date().toISOString();
		if (!isCollector(value)) throw new Error("APP_DIAGNOSTICS_INVALID_PAYLOAD");
		return value;
	};
	return { enable, disable, capture };
}
