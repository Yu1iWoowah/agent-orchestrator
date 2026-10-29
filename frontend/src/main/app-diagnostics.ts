import { randomUUID } from "node:crypto";
import {
	DIAGNOSTIC_CHANNEL,
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
};
type Pending = { request: CollectorRequest; frame: unknown; finish: (error?: string, value?: AppSnapshot) => void };

/** Internal native-consent owner. No IPC endpoint can call allowUntil. */
export class AppDiagnosticsMain {
	private connected = false;
	private adopted = false;
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
	setConnection(value: { connected: boolean; adopted?: boolean }): void {
		const lost = this.connected && !value.connected;
		this.connected = value.connected;
		this.adopted = value.adopted === true;
		if (lost || this.adopted) {
			this.pending?.finish("APP_NOT_CONNECTED");
			clearTimeout(this.expiry);
			this.expiresAt = 0;
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
