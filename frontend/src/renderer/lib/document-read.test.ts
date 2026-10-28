import { QueryClient } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { setApiBaseUrl, setApiDaemonStatus } from "./api-client";
import { documentTargetKey, fetchDocumentCatalog, resolveDocument } from "./document-read";

vi.mock("./telemetry", () => ({ captureRendererEvent: vi.fn().mockResolvedValue(undefined) }));
vi.mock("./sentry", () => ({ captureApiErrorToSentry: vi.fn() }));
const local = { kind: "local", identity: "host-a" } as const;
const target = { kind: "artifact", projectId: "p", sessionId: "p-1", path: "notes hello.md" } as const;
afterEach(() => { vi.restoreAllMocks(); setApiBaseUrl(null); setApiDaemonStatus({ state: "stopped" }); });

describe("document read client", () => {
 it("sends an artifact union to the local daemon and returns the verified location", async () => {
  const seen: Array<{ method: string; path: string; body: unknown }> = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
   const request = new Request(input, init);
   seen.push({ method: request.method, path: new URL(request.url).pathname, body: await request.json() });
   return new Response(JSON.stringify({ document: { target, localPath: "/owned/report.md", canonicalUri: "ao://sessions/p/p-1/artifacts/notes%20hello.md", size: 1, mediaType: "text/markdown", fingerprint: "tag" } }), { headers: { "Content-Type": "application/json" } });
  });
  setApiBaseUrl("http://127.0.0.1:34646");
  const document = await resolveDocument(local, target);
  expect(document.localPath).toBe("/owned/report.md");
  expect(seen).toEqual([{ method: "POST", path: "/api/v1/local-documents/resolve", body: target }]);
 });
 it("does not fall back to a local resolver for remote or Cloud sources", async () => {
  const fetch = vi.spyOn(globalThis, "fetch");
  for (const kind of ["remote", "cloud"] as const) {
   await expect(resolveDocument({ kind, identity: "host-a" }, target)).rejects.toMatchObject({ code: "DOCUMENT_LOCAL_ONLY" });
   await expect(fetchDocumentCatalog({ kind, identity: "host-a" }, "p")).rejects.toMatchObject({ code: "DOCUMENT_LOCAL_ONLY" });
  }
  expect(fetch).not.toHaveBeenCalled();
 });
 it("preserves catalog pagination and structured daemon errors", async () => {
  let url = "";
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
   url = input instanceof Request ? input.url : input.toString();
   return new Response(JSON.stringify({ catalog: { items: [], errors: [{ source: "shared", code: "DOCUMENT_SOURCE_UNAVAILABLE" }], incomplete: true, nextCursor: "next", revision: "rev" } }), { headers: { "Content-Type": "application/json" } });
  });
  setApiBaseUrl("http://127.0.0.1:34646");
  expect(await fetchDocumentCatalog(local, "p", { source: "all", archive: "archive", cursor: "previous", limit: 10 })).toMatchObject({ incomplete: true, nextCursor: "next" });
  expect(new URL(url).searchParams.get("archive")).toBe("archive");
  expect(new URL(url).searchParams.get("cursor")).toBe("previous");
  fetch.mockResolvedValue(new Response(JSON.stringify({ error: "conflict", code: "DOCUMENT_CATALOG_CHANGED", message: "Refresh the catalog", requestId: "request-1" }), { status: 409, headers: { "Content-Type": "application/json" } }));
  await expect(fetchDocumentCatalog(local, "p")).rejects.toMatchObject({ code: "DOCUMENT_CATALOG_CHANGED", requestId: "request-1" });
 });
 it("separates connection, project and source in metadata identity", () => {
  const base = documentTargetKey(local, target);
  expect(base).not.toEqual(documentTargetKey({ kind: "local", identity: "host-b" }, target));
  expect(base).not.toEqual(documentTargetKey({ kind: "remote", identity: "host-a" }, target));
  expect(base).not.toEqual(documentTargetKey(local, { ...target, projectId: "other" }));
  expect(base).not.toEqual(documentTargetKey(local, { kind: "shared", projectId: "p", path: target.path }));
 });
});

it("keeps an explicit top fragment separate from cached unanchored metadata", () => {
 const cache = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity } } });
 cache.setQueryData(documentTargetKey(local, target), { target, canonicalUri: "ao://sessions/p/p-1/artifacts/notes%20hello.md" });
 expect(cache.getQueryData(documentTargetKey(local, { ...target, fragment: "" }))).toBeUndefined();
 cache.clear();
});
