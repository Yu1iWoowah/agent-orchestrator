import type { components, operations } from "../../api/schema";
import { appI18n } from "../i18n";
import { apiClient, apiErrorMessage } from "./api-client";

export type DocumentTarget = components["schemas"]["DocumentTarget"];
export type DocumentInput = components["schemas"]["ResolveDocumentRequest"];
export type DocumentCatalogQuery = NonNullable<operations["getProjectDocumentCatalog"]["parameters"]["query"]>;
export type DocumentConnection = { kind: "local" | "remote" | "cloud"; identity: string };

export class DocumentReadError extends Error {
	constructor(readonly code: string, message: string, readonly requestId?: string) { super(message); }
}
function requireLocal(connection: DocumentConnection, message: string) {
	if (connection.kind !== "local") throw new DocumentReadError("DOCUMENT_LOCAL_ONLY", message);
}

/** Resolves paths only on this desktop; callers must retain the source connection. */
export async function resolveDocument(connection: DocumentConnection, target: DocumentInput, signal?: AbortSignal) {
	requireLocal(connection, appI18n.t("documents.connection.localOnly"));
	const { data, error } = await apiClient.POST("/api/v1/local-documents/resolve", { body: target, signal });
	if (
		(error?.code === "DOCUMENT_UNAVAILABLE" ||
			error?.code === "DOCUMENT_SOURCE_UNAVAILABLE") &&
		target.kind === "shared"
	) {
		let notCreated = false;
		try {
			const catalog = await fetchDocumentCatalog(
				connection,
				target.projectId,
				{ source: "shared", limit: 1 },
				signal,
			);
			notCreated = catalog.errors.some(
				(source) =>
					source.source === "shared" &&
					source.code === "DOCUMENT_SOURCE_MISSING",
			);
		} catch {
			/* Retain the resolver error if the catalog cannot establish source state. */
		}
		if (notCreated)
			throw new DocumentReadError(
				"DOCUMENT_SOURCE_MISSING",
				appI18n.t("documents.sheet.notCreated"),
				error.requestId,
			);
	}
	if (error) throw new DocumentReadError(error.code, apiErrorMessage(error, appI18n.t("documents.read.error")), error.requestId);
	if (!data) throw new DocumentReadError("DOCUMENT_RESPONSE_MISSING", appI18n.t("documents.read.error"));
	return data.document;
}

export async function fetchDocumentCatalog(connection: DocumentConnection, projectId: string, query: DocumentCatalogQuery = {}, signal?: AbortSignal) {
	requireLocal(connection, appI18n.t("documents.catalog.localOnly"));
	const { data, error } = await apiClient.GET("/api/v1/projects/{id}/document-catalog", { params: { path: { id: projectId }, query }, signal });
	if (error) throw new DocumentReadError(error.code, apiErrorMessage(error, appI18n.t("documents.read.error")), error.requestId);
	if (!data) throw new DocumentReadError("DOCUMENT_RESPONSE_MISSING", appI18n.t("documents.read.error"));
	return data.catalog;
}

/** Metadata cache identities never collide across hosts, projects or source kinds. */
export function documentTargetKey(connection: DocumentConnection, target: DocumentInput) {
	const source = target.kind === "local-path" ? [target.value]
		: target.kind === "artifact" ? [target.projectId, target.sessionId, target.path, target.fragment ?? null]
			: [target.projectId, target.path, target.fragment ?? null];
	return ["document-resolve", connection.kind, connection.identity, target.kind, ...source] as const;
}

/** Content stays raw; only the safe reader chooses how to display these bytes. */
export async function readDocumentContent(
	connection: DocumentConnection,
	target: DocumentTarget,
	maxBytes: number,
	fingerprint?: string,
	signal?: AbortSignal,
): Promise<ArrayBuffer> {
	requireLocal(connection, appI18n.t("documents.connection.localOnly"));
	const { data, error } = await apiClient.POST(
		"/api/v1/local-documents/content",
		{
			body: target,
			params: { query: { maxBytes, fingerprint } },
			signal,
			parseAs: "arrayBuffer",
		},
	);
	if (error)
		throw new DocumentReadError(
			error.code,
			apiErrorMessage(error, appI18n.t("documents.read.error")),
			error.requestId,
		);
	if (!data)
		throw new DocumentReadError(
			"DOCUMENT_RESPONSE_MISSING",
			appI18n.t("documents.read.error"),
		);
	return data;
}
