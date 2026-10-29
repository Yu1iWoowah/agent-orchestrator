import type { components } from "../api/schema";

export type AppSnapshot = components["schemas"]["AppDiagnosticsSnapshot"];
export type Collector = NonNullable<AppSnapshot["collector"]>;
export type ComponentObservation = Collector["components"]["entries"][number];
export type WorkspaceObservation = ComponentObservation["workspaces"][number];
export type SessionObservation = WorkspaceObservation["sessions"][number];
export type GeometryNode = Collector["geometry"]["nodes"][number];
export type GeometryStyle = NonNullable<GeometryNode["style"]>;
export type QueryObservation = Collector["cache"]["queries"][number];
export const DIAGNOSTIC_MAX_BYTES = 1 << 20;
export const DIAGNOSTIC_CHANNEL = {
	status: "appDiagnostics:status",
	identity: "appDiagnostics:identity",
	consent: "appDiagnostics:consent",
	audit: "appDiagnostics:audit",
	enable: "appDiagnostics:enable",
	revoke: "appDiagnostics:revoke",
	register: "appDiagnostics:register",
	configure: "appDiagnostics:configure",
	request: "appDiagnostics:request",
	submit: "appDiagnostics:submit",
	cancel: "appDiagnostics:cancel",
} as const;

// Only fixed status codes cross IPC. In particular, no handshake token does.
export type DiagnosticsStatus = {
	state: "disabled" | "ready" | "unavailable";
	reason: "consent_required" | "adopted_daemon" | "no_window" | "not_connected" | "unsupported" | "renderer_unavailable" | "ready";
};
export type DiagnosticDocument = { documentId: string; windowId: string; generation: number; enabled: boolean; expiresAt: number };
export type CollectorRequest = {
	schemaVersion: 1;
	requestId: string;
	documentId: string;
	windowId: string;
	generation: number;
	projectId: string;
	deadline: number;
};
export const COLLECTOR_FAILURE_CODES = [
	"APP_DIAGNOSTICS_DISABLED",
	"APP_DIAGNOSTICS_INVALID_PAYLOAD",
	"APP_DIAGNOSTICS_PAYLOAD_TOO_LARGE",
	"APP_RENDERER_UNAVAILABLE",
] as const;
export type CollectorFailureCode = (typeof COLLECTOR_FAILURE_CODES)[number];
type ReplyIdentity = { schemaVersion: 1; requestId: string; documentId: string; windowId: string; generation: number };
export type CollectorReply = ReplyIdentity & ({ collector: Collector } | { code: CollectorFailureCode });

type Rule = (value: unknown) => boolean;
const number: Rule = (v) => typeof v === "number" && Number.isFinite(v) && Math.abs(v) <= 1e6;
const count: Rule = (v) => typeof v === "number" && Number.isSafeInteger(v) && v >= 0 && v <= 1e7;
const revision: Rule = (v) => typeof v === "number" && Number.isSafeInteger(v) && v >= 0;
const boolean: Rule = (v) => typeof v === "boolean";
export const diagnosticId: Rule = (v) => typeof v === "string" && /^[A-Za-z0-9._:-]{1,128}$/.test(v);
const maybeId: Rule = (v) => v === "" || diagnosticId(v);
const timestamp: Rule = (v) =>
	typeof v === "string" && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(v) && Number.isFinite(Date.parse(v));
const milliseconds: Rule = (v) => typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 2e13;
const oneOf =
	(values: string): Rule =>
	(v) =>
		typeof v === "string" && values.split(",").includes(v);
const array =
	(item: Rule, max: number): Rule =>
	(v) =>
		Array.isArray(v) && v.length <= max && v.every(item);
const optional =
	(item: Rule): Rule =>
	(v) =>
		v === undefined || item(v);
const object =
	(fields: Record<string, Rule>): Rule =>
	(v) => {
		if (typeof v !== "object" || v === null || Array.isArray(v) || ![Object.prototype, null].includes(Object.getPrototypeOf(v)))
			return false;
		const descriptors = Object.getOwnPropertyDescriptors(v);
		return (
			Object.keys(descriptors).every((key) => Object.hasOwn(fields, key) && "value" in descriptors[key]) &&
			Object.entries(fields).every(([key, rule]) => rule(Object.hasOwn(descriptors, key) ? descriptors[key].value : undefined))
		);
	};
const availability = oneOf("available,unavailable,not_mounted,unsupported_surface,not_recorded_before_enable,disabled");
const source = oneOf("local,remote,cloud");
const provenance = { availability, observedAt: timestamp, revision, omittedCount: count };
const ids = array(diagnosticId, 200);
const session = object({
	id: diagnosticId,
	kind: oneOf("worker,orchestrator,unknown"),
	terminated: boolean,
	status: oneOf(
		"spawning,working,pr_open,draft,ci_failed,review_pending,changes_requested,commented,approved,mergeable,ready_to_merge,merged,cleanup,done,exited,killed,stuck,needs_input,no_signal,idle,terminated,unknown",
	),
	activity: oneOf("active,idle,waiting_input,blocked,exited,unknown"),
	column: oneOf("building,validating,needs_review,needs_input,ready,done,archive,unknown"),
});
const workspaceShape = object({
	scope: oneOf("ids,counts"),
	projectId: diagnosticId,
	hostId: maybeId,
	source,
	shared: boolean,
	folderMissing: boolean,
	sessionCount: count,
	sessions: array(session, 200),
	omittedCount: count,
});
const workspace: Rule = (v) =>
	workspaceShape(v) && (Reflect.get(Object(v), "scope") === "ids" || Reflect.get(Object(v), "sessions").length === 0);
const workspaces = array(workspace, 100);
const component = object({
	omittedCount: count,
	kind: oneOf("sidebar,project,board,session,chat,terminal,error-boundary"),
	revision: (v) => revision(v) && v !== 0,
	observedAt: timestamp,
	projectId: maybeId,
	hostId: maybeId,
	source,
	expanded: boolean,
	shared: boolean,
	showAll: boolean,
	sessionCount: count,
	hiddenCount: count,
	liveIds: ids,
	workerIds: ids,
	orderedIds: ids,
	listedIds: ids,
	workspaces,
});
const queryShape = object({
	kind: oneOf("workspaces,cloud-projects,cloud-sessions,cloud-shared-workspaces,remote-workspaces,remote-projects,remote-sessions"),
	hostId: maybeId,
	status: oneOf("pending,error,success"),
	fetchStatus: oneOf("fetching,paused,idle"),
	invalidated: boolean,
	dataUpdatedAt: milliseconds,
	errorUpdatedAt: milliseconds,
	observerCount: count,
	failureCount: count,
	count,
	workspaces,
});
const query: Rule = (v) =>
	queryShape(v) && (Reflect.get(Object(v), "kind") === "workspaces" || Reflect.get(Object(v), "workspaces").length === 0);
const box = object({ x: number, y: number, width: (v) => number(v) && Number(v) >= 0, height: (v) => number(v) && Number(v) >= 0 });
const overflow = oneOf("visible,hidden,clip,auto,scroll,other");
const style = object({
	box,
	display: oneOf("none,block,flex,grid,inline,inline-block,inline-flex,inline-grid,contents,table,other"),
	visibility: oneOf("visible,hidden,collapse,other"),
	opacity: (v) => number(v) && Number(v) >= 0 && Number(v) <= 1,
	overflowX: overflow,
	overflowY: overflow,
	clip: oneOf("none,inset,other"),
	gridRows: array(number, 64),
	transform: array(number, 16),
	zIndex: number,
});
const nodeShape = object({
	source,
	hostId: maybeId,
	id: diagnosticId,
	role: oneOf("sidebar,project_root,session_list,session_row,main_pane"),
	projectId: maybeId,
	sessionId: maybeId,
	availability: oneOf("available,not_mounted,unavailable"),
	style: optional(style),
	ancestors: array(style, 8),
	intersectsViewport: boolean,
	hit: oneOf("clear,registered,unregistered_overlay,not_tested"),
});
const node: Rule = (v) =>
	nodeShape(v) && (Reflect.get(Object(v), "availability") !== "available" || style(Reflect.get(Object(v), "style")));
const error = object({
	at: timestamp,
	source: oneOf("console_error,window_error,unhandled_rejection"),
	name: oneOf("Error,TypeError,RangeError,ReferenceError,SyntaxError,Unknown"),
	message: (v) => v === "[redacted]",
	count: (v) => count(v) && v !== 0,
});
const collectorShape = object({
	redactionSchemaVersion: (v) => v === 1,
	captureStartedAt: timestamp,
	captureFinishedAt: timestamp,
	consistency: oneOf("stable,changed"),
	route: object({
		...provenance,
		routeId: oneOf("home,board,session,settings,terminal,unsupported_surface"),
		source: oneOf("local,remote,cloud,unknown"),
		projectId: maybeId,
		sessionId: maybeId,
		hostId: maybeId,
	}),
	shell: object({
		...provenance,
		width: (v) => number(v) && Number(v) >= 0,
		height: (v) => number(v) && Number(v) >= 0,
		devicePixelRatio: (v) => number(v) && Number(v) > 0 && Number(v) <= 16,
		sidebarCollapsed: boolean,
		sidebarWidth: (v) => number(v) && Number(v) >= 0,
		inspectorOpen: boolean,
	}),
	cache: object({ ...provenance, queries: array(query, 100) }),
	components: object({ ...provenance, entries: array(component, 102) }),
	geometry: object({ ...provenance, ancestorTruncatedCount: count, nodes: array(node, 1000) }),
	errors: object({ ...provenance, enabledAt: timestamp, notRecordedBeforeEnable: (v) => v === true, records: array(error, 100) }),
});
const version: Rule = (v) => v === "unknown" || (typeof v === "string" && /^\d+\.\d+\.\d+([.-][A-Za-z0-9.-]+)?$/.test(v));
const identity = object({
	appVersion: version,
	daemonVersion: version,
	buildCommit: (v) => v === "unknown" || (typeof v === "string" && /^[0-9a-f]{40}$/.test(v)),
	os: oneOf("linux,darwin,win32,unknown"),
});
function bounded(value: unknown, rule: Rule): boolean {
	try {
		return rule(value) && new TextEncoder().encode(JSON.stringify(value)).length <= DIAGNOSTIC_MAX_BYTES;
	} catch {
		return false;
	}
}
export function isCollector(value: unknown): value is Collector {
	if (!bounded(value, collectorShape)) return false;
	return Date.parse(Reflect.get(Object(value), "captureFinishedAt")) >= Date.parse(Reflect.get(Object(value), "captureStartedAt"));
}
const snapshotShape = object({
	schemaVersion: (v) => v === 1,
	instanceId: diagnosticId,
	windowId: diagnosticId,
	rendererGeneration: (v) => revision(v) && v !== 0,
	capturedAt: timestamp,
	completeness: oneOf("complete,partial"),
	collector: isCollector,
	identity,
});
export function isAppSnapshot(value: unknown): value is AppSnapshot {
	if (!bounded(value, snapshotShape)) return false;
	if (Reflect.get(Object(value), "completeness") !== "complete") return true;
	const collector = Reflect.get(Object(value), "collector");
	return (
		collector.consistency === "stable" &&
		[collector.route, collector.shell, collector.cache, collector.components, collector.geometry, collector.errors].every(
			(section) => section.availability === "available" && section.omittedCount === 0,
		)
	);
}
export function isCollectorRequest(value: unknown): value is CollectorRequest {
	return bounded(
		value,
		object({
			schemaVersion: (v: unknown) => v === 1,
			requestId: diagnosticId,
			documentId: diagnosticId,
			windowId: diagnosticId,
			generation: (v) => revision(v) && v !== 0,
			projectId: maybeId,
			deadline: milliseconds,
		}),
	);
}
export function isCollectorReply(value: unknown): value is CollectorReply {
	const identity = {
		schemaVersion: (v: unknown) => v === 1,
		requestId: diagnosticId,
		documentId: diagnosticId,
		windowId: diagnosticId,
		generation: (v: unknown) => revision(v) && v !== 0,
	};
	return (
		bounded(value, object({ ...identity, collector: isCollector })) ||
		bounded(value, object({ ...identity, code: oneOf(COLLECTOR_FAILURE_CODES.join(",")) }))
	);
}

export function isDiagnosticDocument(value: unknown): value is DiagnosticDocument {
	return bounded(
		value,
		object({
			documentId: diagnosticId,
			windowId: diagnosticId,
			generation: (v) => revision(v) && v !== 0,
			enabled: boolean,
			expiresAt: milliseconds,
		}),
	);
}
export function isDiagnosticsStatus(value: unknown): value is DiagnosticsStatus {
	return bounded(
		value,
		object({
			state: oneOf("disabled,ready,unavailable"),
			reason: oneOf("consent_required,adopted_daemon,no_window,not_connected,unsupported,renderer_unavailable,ready"),
		}),
	);
}
export function isDiagnosticCancellation(value: unknown): value is { requestId: string; documentId: string } {
	return bounded(value, object({ requestId: diagnosticId, documentId: diagnosticId }));
}

export type DiagnosticConsent = { grantId: string; sessionId: string; expiresAt: number };
export type ConsentRequest = Pick<CollectorRequest, "schemaVersion" | "requestId" | "documentId" | "windowId" | "generation">;
export type EnableRequest = ConsentRequest & { sessionId: string };
export type EnableResult = EnableRequest & ({ outcome: "cancelled" } | ({ outcome: "enabled" } & DiagnosticConsent));
const consentIdentity = {
	schemaVersion: (v: unknown) => v === 1,
	requestId: diagnosticId,
	documentId: diagnosticId,
	windowId: diagnosticId,
	generation: (v: unknown) => revision(v) && Number(v) > 0,
};
export const isConsentRequest = (v: unknown): v is ConsentRequest => object(consentIdentity)(v);
export const isEnableRequest = (v: unknown): v is EnableRequest => object({ ...consentIdentity, sessionId: diagnosticId })(v);
export const isDiagnosticConsent = (v: unknown): v is DiagnosticConsent =>
	object({ grantId: diagnosticId, sessionId: diagnosticId, expiresAt: milliseconds })(v);
export const isEnableResult = (v: unknown): v is EnableResult =>
	object({ ...consentIdentity, sessionId: diagnosticId, outcome: (v: unknown) => v === "cancelled" })(v) ||
	object({
		...consentIdentity,
		outcome: (v: unknown) => v === "enabled",
		grantId: diagnosticId,
		sessionId: diagnosticId,
		expiresAt: milliseconds,
	})(v);
export const sameConsentIdentity = (request: ConsentRequest, reply: ConsentRequest): boolean =>
	request.schemaVersion === reply.schemaVersion &&
	request.requestId === reply.requestId &&
	request.documentId === reply.documentId &&
	request.windowId === reply.windowId &&
	request.generation === reply.generation;
export type ConsentAck = ConsentRequest & { revoked: true };
export const isConsentAck = (v: unknown): v is ConsentAck => object({ ...consentIdentity, revoked: (v: unknown) => v === true })(v);

export type DiagnosticAudit = {
	records: {
		at: string;
		outcome: "granted" | "accepted" | "completed" | "failed" | "interrupted" | "revoked" | "read" | "list" | "audit_read" | "unknown";
		bytes: number;
		partial: boolean;
	}[];
	partial: boolean;
	droppedCount: number;
};
export const isDiagnosticAudit = (v: unknown): v is DiagnosticAudit =>
	object({
		records: array(
			object({
				at: timestamp,
				outcome: oneOf("granted,accepted,completed,failed,interrupted,revoked,read,list,audit_read,unknown"),
				bytes: count,
				partial: boolean,
			}),
			100,
		),
		partial: boolean,
		droppedCount: revision,
	})(v);

export type AuditResponse = ConsentRequest & { audit: DiagnosticAudit };
export const isAuditResponse = (v: unknown): v is AuditResponse => object({ ...consentIdentity, audit: isDiagnosticAudit })(v);
