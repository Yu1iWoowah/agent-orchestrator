import { randomUUID } from "node:crypto";
import {
	DIAGNOSTIC_CHANNEL,
	isConsentRequest,
	isEnableRequest,
	type ConsentRequest,
	type DiagnosticConsent,
	type DiagnosticAudit,
	type EnableResult,
	type ConsentAck,
	type AuditResponse,
	isAppSnapshot,
	isCollectorReply,
	type AppSnapshot,
	type CollectorRequest,
	type DiagnosticDocument,
	type DiagnosticsStatus,
} from "../shared/app-diagnostics";
export type DiagnosticsShell = {
	id: number;
	mainFrame: { url: string };
	isDestroyed: () => boolean;
	send: (channel: string, value: unknown) => void;
};
export type DiagnosticsEvent = { sender: unknown; senderFrame: unknown };
export type DiagnosticsMainOptions = {
	instanceId: string;
	rendererURL: () => string;
	shell: () => DiagnosticsShell | null;
	identity: () => NonNullable<AppSnapshot["identity"]>;
	changed?: () => void;
	consentCleared?: () => void;
	consent?: {
		confirm: (sessionId: string) => Promise<boolean>;
		grant: (sessionId: string) => Promise<string>;
		revoke: (grantId: string) => Promise<void>;
		audit?: (grantId: string) => Promise<DiagnosticAudit>;
	};
};
type Pending = { request: CollectorRequest; frame: unknown; finish: (error?: string, value?: AppSnapshot) => void };

/** Internal native-consent owner. No IPC endpoint can call allowUntil. */
export class AppDiagnosticsMain {
	private grant: DiagnosticConsent | null = null;
	private consenting = false;
	private consentRequests = new Set<string>();
	private connected = false;
	private adopted = false;
	private unsupported = false;
	private connectionGeneration = 0;
	private expiresAt = 0;
	private expiry: ReturnType<typeof setTimeout> | undefined;
	private registered = false;
	private generation = 1;
	private documentId: string = randomUUID();
	private pending: Pending | undefined;
	constructor(private readonly options: DiagnosticsMainOptions) {}
	private shell(): DiagnosticsShell | null {
		const shell = this.options.shell();
		return shell && !shell.isDestroyed() ? shell : null;
	}
	private enabled(): boolean {
		return this.connected && !this.adopted && this.expiresAt > Date.now();
	}
	status(): DiagnosticsStatus {
		if (!this.shell()) return { state: "unavailable", reason: "no_window" };
		if (this.adopted) return { state: "unavailable", reason: "adopted_daemon" };
		if (this.unsupported) return { state: "unavailable", reason: "unsupported" };
		if (!this.connected) return { state: "unavailable", reason: "not_connected" };
		if (!this.enabled()) return { state: "disabled", reason: "consent_required" };
		if (!this.registered) return { state: "unavailable", reason: "renderer_unavailable" };
		return { state: "ready", reason: "ready" };
	}
	private document(): DiagnosticDocument {
		return {
			documentId: this.documentId,
			windowId: `window-${this.shell()?.id ?? 0}`,
			generation: this.generation,
			enabled: this.enabled() && this.registered,
			expiresAt: this.expiresAt,
		};
	}
	private publish(): void {
		this.shell()?.send(DIAGNOSTIC_CHANNEL.status, this.status());
		this.shell()?.send(DIAGNOSTIC_CHANNEL.configure, this.document());
		this.options.changed?.();
	}
	setConnection(value: { connected: boolean; adopted?: boolean; unsupported?: boolean }): void {
		const lost = this.connected && !value.connected;
		if (this.connected !== value.connected) this.connectionGeneration++;
		this.connected = value.connected;
		this.adopted = value.adopted === true;
		this.unsupported = value.unsupported === true;
		if (lost || this.adopted) {
			this.pending?.finish("APP_NOT_CONNECTED");
			clearTimeout(this.expiry);
			this.expiresAt = 0;
			this.grant = null;
		}
		this.publish();
	}
	allowUntil(expiresAt: number): void {
		if (expiresAt && (!this.connected || this.adopted)) throw new Error("APP_NOT_CONNECTED");
		if (!Number.isFinite(expiresAt) || expiresAt < 0) throw new Error("APP_DIAGNOSTICS_INVALID_ARGUMENT");
		const hadConsent = this.expiresAt > 0;
		clearTimeout(this.expiry);
		this.expiresAt = expiresAt ? Math.min(expiresAt, Date.now() + 900_000) : 0;
		if (this.expiresAt > Date.now()) this.expiry = setTimeout(() => this.allowUntil(0), this.expiresAt - Date.now());
		else {
			this.expiresAt = 0;
			this.grant = null;
			this.pending?.finish("APP_DIAGNOSTICS_DISABLED");
		}
		this.publish();
		if (hadConsent && !this.expiresAt) this.options.consentCleared?.();
	}
	private trust(event: DiagnosticsEvent): DiagnosticsShell {
		const shell = this.shell();
		if (!shell || event.sender !== shell || event.senderFrame !== shell.mainFrame) throw new Error("APP_DIAGNOSTICS_UNTRUSTED_FRAME");
		try {
			const actual = new URL(shell.mainFrame.url);
			const expected = new URL(this.options.rendererURL());
			if (actual.protocol !== expected.protocol || actual.host !== expected.host || actual.pathname !== expected.pathname)
				throw new Error();
		} catch {
			throw new Error("APP_DIAGNOSTICS_UNTRUSTED_FRAME");
		}
		return shell;
	}
	readStatus(event: DiagnosticsEvent): DiagnosticsStatus {
		this.trust(event);
		return this.status();
	}
	register(event: DiagnosticsEvent): DiagnosticDocument {
		this.trust(event);
		this.registered = true;
		this.publish();
		return this.document();
	}
	readConsent(event: DiagnosticsEvent): DiagnosticConsent | null {
		this.trust(event);
		return this.enabled() && this.grant ? { ...this.grant } : null;
	}
	private consentIdentity(event: DiagnosticsEvent, request: ConsentRequest): void {
		this.trust(event);
		if (
			request.documentId !== this.documentId ||
			request.generation !== this.generation ||
			request.windowId !== `window-${this.shell()?.id}`
		)
			throw new Error("APP_RENDERER_CHANGED");
	}
	private consumeConsent(request: ConsentRequest): void {
		if (this.consentRequests.has(request.requestId) || this.consentRequests.size >= 1024)
			throw new Error("APP_DIAGNOSTICS_INVALID_ARGUMENT");
		this.consentRequests.add(request.requestId);
	}
	async requestEnable(event: DiagnosticsEvent, value: unknown): Promise<EnableResult> {
		this.trust(event);
		if (!isEnableRequest(value)) throw new Error("APP_DIAGNOSTICS_INVALID_ARGUMENT");
		this.consentIdentity(event, value);
		this.consumeConsent(value);
		const consent = this.options.consent;
		if (!consent || !this.connected || this.adopted || !this.registered) throw new Error("APP_DIAGNOSTICS_UNSUPPORTED");
		if (this.consenting || this.grant) throw new Error("APP_DIAGNOSTICS_BUSY");
		this.consenting = true;
		const connection = this.connectionGeneration;
		try {
			if (!(await consent.confirm(value.sessionId))) return { ...value, outcome: "cancelled" };
			this.consentIdentity(event, value);
			if (!this.connected || connection !== this.connectionGeneration) throw new Error("APP_NOT_CONNECTED");
			const expiresAt = Date.now() + 900_000;
			this.allowUntil(expiresAt);
			const grantId = await consent.grant(value.sessionId);
			try {
				this.consentIdentity(event, value);
				if (!this.enabled() || connection !== this.connectionGeneration) throw new Error("APP_NOT_CONNECTED");
			} catch (error) {
				await consent.revoke(grantId);
				throw error;
			}
			this.grant = { grantId, sessionId: value.sessionId, expiresAt };
			this.publish();
			return { ...value, outcome: "enabled", ...this.grant };
		} catch (error) {
			this.allowUntil(0);
			const codes = new Set([
				"APP_RENDERER_CHANGED",
				"APP_NOT_CONNECTED",
				"APP_DIAGNOSTICS_DISABLED",
				"APP_DIAGNOSTICS_UNSUPPORTED",
				"APP_DIAGNOSTICS_CAPABILITY_INVALID",
				"APP_DIAGNOSTICS_STORAGE_FAILED",
				"APP_DIAGNOSTICS_AUDIT_FAILED",
				"APP_DIAGNOSTICS_SESSION_UNAVAILABLE",
			]);
			throw new Error(error instanceof Error && codes.has(error.message) ? error.message : "APP_DIAGNOSTICS_UNSUPPORTED");
		} finally {
			this.consenting = false;
		}
	}
	async readAudit(event: DiagnosticsEvent, value: unknown): Promise<AuditResponse> {
		this.trust(event);
		if (!isConsentRequest(value)) throw new Error("APP_DIAGNOSTICS_INVALID_ARGUMENT");
		this.consentIdentity(event, value);
		this.consumeConsent(value);
		const grant = this.readConsent(event);
		if (!grant || !this.options.consent?.audit) throw new Error("APP_DIAGNOSTICS_DISABLED");
		const result = await this.options.consent.audit(grant.grantId);
		this.consentIdentity(event, value);
		if (this.readConsent(event)?.grantId !== grant.grantId) throw new Error("APP_DIAGNOSTICS_DISABLED");
		return { ...value, audit: result };
	}

	grantRevoked(grantId: string): void {
		if (this.grant?.grantId === grantId || (this.consenting && this.enabled())) this.allowUntil(0);
	}

	async requestRevoke(event: DiagnosticsEvent, value: unknown): Promise<ConsentAck> {
		this.trust(event);
		if (!isConsentRequest(value)) throw new Error("APP_DIAGNOSTICS_INVALID_ARGUMENT");
		this.consentIdentity(event, value);
		this.consumeConsent(value);
		const grant = this.grant;
		// Stop collection synchronously; retain the channel until revoke is acknowledged.
		clearTimeout(this.expiry);
		this.expiresAt = 0;
		this.grant = null;
		this.pending?.finish("APP_DIAGNOSTICS_DISABLED");
		this.publish();
		try {
			if (grant) await this.options.consent?.revoke(grant.grantId);
			return { ...value, revoked: true };
		} finally {
			this.options.consentCleared?.();
		}
	}

	submit(event: DiagnosticsEvent, value: unknown): boolean {
		this.trust(event);
		const pending = this.pending;
		if (!pending || typeof value !== "object" || value === null || Reflect.get(value, "requestId") !== pending.request.requestId)
			return false;
		if (
			Reflect.get(value, "documentId") !== pending.request.documentId ||
			Reflect.get(value, "windowId") !== pending.request.windowId ||
			Reflect.get(value, "generation") !== pending.request.generation
		)
			return false;
		if (event.senderFrame !== pending.frame || this.documentId !== pending.request.documentId) return false;
		if (!this.enabled() || Date.now() > pending.request.deadline) {
			pending.finish("APP_SNAPSHOT_TIMEOUT");
			return false;
		}
		if (!isCollectorReply(value)) {
			pending.finish("APP_DIAGNOSTICS_INVALID_PAYLOAD");
			return false;
		}
		if ("code" in value) {
			pending.finish(value.code);
			return true;
		}
		const c = value.collector;
		const complete =
			c.consistency === "stable" &&
			[c.route, c.shell, c.cache, c.components, c.geometry, c.errors].every(
				(section) => section.availability === "available" && section.omittedCount === 0,
			);
		const snapshot: AppSnapshot = {
			schemaVersion: 1,
			instanceId: this.options.instanceId,
			windowId: pending.request.windowId,
			rendererGeneration: pending.request.generation,
			capturedAt: new Date().toISOString(),
			completeness: complete ? "complete" : "partial",
			identity: this.options.identity(),
			collector: c,
		};
		if (!isAppSnapshot(snapshot)) {
			pending.finish("APP_DIAGNOSTICS_INVALID_PAYLOAD");
			return false;
		}
		pending.finish(undefined, snapshot);
		return true;
	}
	async capture(request: { windowId?: string; projectId?: string }, signal: AbortSignal): Promise<AppSnapshot> {
		const shell = this.shell();
		if (!shell) throw new Error("APP_WINDOW_GONE");
		if (!this.enabled()) throw new Error("APP_DIAGNOSTICS_DISABLED");
		if (!this.registered) throw new Error("APP_RENDERER_UNAVAILABLE");
		if (request.windowId && request.windowId !== `window-${shell.id}`) throw new Error("APP_WINDOW_GONE");
		if (this.pending) throw new Error("APP_DIAGNOSTICS_BUSY");
		if (signal.aborted) throw new Error("APP_SNAPSHOT_TIMEOUT");
		return new Promise<AppSnapshot>((resolve, reject) => {
			const incoming: CollectorRequest = {
				schemaVersion: 1,
				requestId: randomUUID(),
				documentId: this.documentId,
				windowId: `window-${shell.id}`,
				generation: this.generation,
				projectId: request.projectId ?? "",
				deadline: Date.now() + 5000,
			};
			const cancel = () => finish("APP_SNAPSHOT_TIMEOUT");
			const timer = setTimeout(() => finish("APP_SNAPSHOT_TIMEOUT"), 5000);
			const finish = (error?: string, snapshot?: AppSnapshot) => {
				if (this.pending?.request !== incoming) return;
				this.pending = undefined;
				clearTimeout(timer);
				signal.removeEventListener("abort", cancel);
				if (error) {
					this.shell()?.send(DIAGNOSTIC_CHANNEL.cancel, { requestId: incoming.requestId, documentId: incoming.documentId });
					reject(new Error(error));
				} else if (snapshot) resolve(snapshot);
			};
			this.pending = { request: incoming, frame: shell.mainFrame, finish };
			signal.addEventListener("abort", cancel, { once: true });
			try {
				shell.send(DIAGNOSTIC_CHANNEL.request, incoming);
			} catch {
				finish("APP_WINDOW_GONE");
			}
		});
	}
	resetDocument(code = "APP_RENDERER_CHANGED"): void {
		this.pending?.finish(code);
		this.registered = false;
		this.generation++;
		this.documentId = randomUUID();
		this.consentRequests.clear();
		this.publish();
	}
	windows(): { id: string; generation: number }[] {
		const shell = this.shell();
		return shell && this.enabled() && this.registered ? [{ id: `window-${shell.id}`, generation: this.generation }] : [];
	}
	dispose(): void {
		this.pending?.finish("APP_NOT_CONNECTED");
		clearTimeout(this.expiry);
		this.expiresAt = 0;
		this.connected = false;
		this.registered = false;
	}
}
export type { CollectorRequest };
