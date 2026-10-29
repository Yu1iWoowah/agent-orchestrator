import type { QueryClient } from "@tanstack/react-query";
import type { AoBridge } from "../../preload";
import { COLLECTOR_FAILURE_CODES, type CollectorFailureCode, type DiagnosticDocument } from "../../shared/app-diagnostics";
import { createAppDiagnosticsCollector, type RouteObservation } from "./app-diagnostics";

/** Records only after main's native-consent owner explicitly configures this document. */
export function startAppDiagnosticsRenderer(options: {
	bridge: AoBridge["appDiagnostics"] | undefined;
	queryClient: QueryClient;
	readRoute: () => RouteObservation;
	observeRoute?: () => () => void;
	readInspectorOpen?: (route: RouteObservation) => boolean;
}): () => void {
	const bridge = options.bridge;
	if (!bridge) return () => undefined;
	const collector = createAppDiagnosticsCollector(options);
	let document: DiagnosticDocument | undefined;
	let disposed = false;
	let expiry: ReturnType<typeof setTimeout> | undefined;
	const seen = new Set<string>();
	const configure = (next: DiagnosticDocument) => {
		if (disposed) return;
		clearTimeout(expiry);
		const changed = document?.documentId !== next.documentId || !next.enabled;
		document = next;
		if (changed) {
			seen.clear();
			collector.disable();
		}
		if (next.enabled && next.expiresAt > Date.now()) {
			collector.enable();
			expiry = setTimeout(() => {
				collector.disable();
			}, next.expiresAt - Date.now());
		} else collector.disable();
	};
	let configurationRevision = 0;
	const stopConfigure = bridge.onConfigure((next) => {
		configurationRevision++;
		configure(next);
	});
	const stopRequest = bridge.onRequest((request) => {
		if (
			disposed ||
			!document?.enabled ||
			document.expiresAt <= Date.now() ||
			request.deadline < Date.now() ||
			request.documentId !== document.documentId ||
			request.windowId !== document.windowId ||
			request.generation !== document.generation ||
			seen.has(request.requestId)
		)
			return;
		if (seen.size >= 1024) seen.clear();
		seen.add(request.requestId);
		try {
			const collectorData = collector.capture(request.projectId);
			void bridge
				.submit({
					requestId: request.requestId,
					documentId: document.documentId,
					windowId: document.windowId,
					generation: document.generation,
					collector: collectorData,
				})
				.catch(() => undefined);
		} catch (error: unknown) {
			const code: CollectorFailureCode =
				error instanceof Error && COLLECTOR_FAILURE_CODES.some((code) => code === error.message)
					? (error.message as CollectorFailureCode)
					: "APP_RENDERER_UNAVAILABLE";
			void bridge
				.submit({
					requestId: request.requestId,
					documentId: document.documentId,
					windowId: document.windowId,
					generation: document.generation,
					code,
				})
				.catch(() => undefined);
		}
	});
	const stopCancel = bridge.onCancel((request) => {
		if (request.documentId === document?.documentId) seen.add(request.requestId);
	});
	const bootstrapRevision = configurationRevision;
	void bridge
		.register()
		.then((next) => {
			if (configurationRevision === bootstrapRevision) configure(next);
		})
		.catch(() => {
			if (configurationRevision === bootstrapRevision) collector.disable();
		});
	return () => {
		disposed = true;
		clearTimeout(expiry);
		stopConfigure();
		stopRequest();
		stopCancel();
		seen.clear();
		collector.disable();
	};
}
