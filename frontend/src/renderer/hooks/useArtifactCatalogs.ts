import { useCallback, useMemo, useSyncExternalStore } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getApiBaseUrl, subscribeApiBaseUrl } from "../lib/api-client";
import { readArtifactCatalogs, type ProjectCatalogResult } from "../lib/artifact-catalog";
import type { DocumentConnection } from "../lib/document-read";
import { STANDALONE_WORKSPACE_ID, type WorkspaceSummary } from "../types/workspace";

export function useArtifactCatalogs(workspaces: readonly WorkspaceSummary[] | undefined, enabled = true) {
	const identity = useSyncExternalStore(subscribeApiBaseUrl, getApiBaseUrl);
	const connection = useMemo<DocumentConnection>(() => ({ kind: "local", identity }), [identity]);
	const projects = useMemo(
		() =>
			(workspaces ?? []).filter(
				(workspace) => !workspace.hostId && workspace.kind !== "cloud" && workspace.id !== STANDALONE_WORKSPACE_ID,
			),
		[workspaces],
	);
	const ids = projects.map((project) => project.id).sort();
	const key = ["artifact-catalogs", connection.kind, identity, ids];
	const client = useQueryClient();
	const cache = client.getQueryCache();
	// A sidebar observes cached rows without keeping the gallery's scan alive.
	const cached = useSyncExternalStore(
		useCallback((notify) => enabled ? () => {} : cache.subscribe(notify), [cache, enabled]),
		() => client.getQueryData<ProjectCatalogResult[]>(key),
	);
	const query = useQuery({
		queryKey: key,
		subscribed: enabled,
		enabled: enabled && workspaces !== undefined,
		queryFn: ({ signal }) => {
			const previous = client.getQueryData<ProjectCatalogResult[]>(key);
			return readArtifactCatalogs(connection, ids, signal, (progress) => {
				client.setQueryData<ProjectCatalogResult[]>(
					key,
					progress.map((project) => {
						const old = previous?.find((entry) => entry.projectId === project.projectId);
						return old && project.loading && project.items.length === 0 ? { ...project, items: old.items } : project;
					}),
				);
			});
		},
		staleTime: 30_000,
		gcTime: 5 * 60_000,
		retry: false,
	});
	return { ...query, data: enabled ? query.data : cached, connection, projects, contextKey: JSON.stringify([connection.kind, identity]) };
}
