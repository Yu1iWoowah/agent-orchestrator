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
function requireLocal(connection: DocumentConnection) {
 if (connection.kind !== "local") throw new DocumentReadError("DOCUMENT_LOCAL_ONLY", appI18n.t("documents.connection.localOnly"));
}

/** Resolves paths only on this desktop; callers must retain the source connection. */
export async function resolveDocument(connection: DocumentConnection, target: DocumentInput, signal?: AbortSignal) {
 requireLocal(connection);
 const { data, error } = await apiClient.POST("/api/v1/local-documents/resolve", { body: target, signal });
 if (error) throw new DocumentReadError(error.code, apiErrorMessage(error, appI18n.t("documents.read.error")), error.requestId);
 if (!data) throw new DocumentReadError("DOCUMENT_RESPONSE_MISSING", appI18n.t("documents.read.error"));
 return data.document;
}

export async function fetchDocumentCatalog(connection: DocumentConnection, projectId: string, query: DocumentCatalogQuery = {}, signal?: AbortSignal) {
 requireLocal(connection);
 const { data, error } = await apiClient.GET("/api/v1/projects/{id}/document-catalog", { params: { path: { id: projectId }, query }, signal });
 if (error) throw new DocumentReadError(error.code, apiErrorMessage(error, appI18n.t("documents.read.error")), error.requestId);
 if (!data) throw new DocumentReadError("DOCUMENT_RESPONSE_MISSING", appI18n.t("documents.read.error"));
 return data.catalog;
}

/** Metadata cache identities never collide across hosts, projects or source kinds. */
export function documentTargetKey(connection: DocumentConnection, target: DocumentInput) {
 const source = target.kind === "local-path" ? [target.value]
  : target.kind === "artifact" ? [target.projectId, target.sessionId, target.path, target.fragment ?? ""]
   : [target.projectId, target.path, target.fragment ?? ""];
 return ["document-resolve", connection.kind, connection.identity, target.kind, ...source] as const;
}
