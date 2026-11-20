import { useQuery } from "@tanstack/react-query";
import { apiClient, hasTrustedApiBaseUrl } from "../lib/api-client";
import { usesPreviewWorkspaceData } from "../lib/preview-mode";
import type { SessionArtifact } from "../types/workspace";
import { toSessionArtifact } from "./useWorkspaceQuery";

export const keptArtifactsQueryKey = ["kept-artifacts"] as const;

/** Read-only artifacts kept when their session was deleted. */
export type KeptArtifactSet = {
	sessionId: string;
	/** The session's project; undefined once it has none. */
	projectId?: string;
	title: string;
	artifactFiles: SessionArtifact[];
};

async function fetchKeptArtifacts(): Promise<KeptArtifactSet[]> {
	const { data, error } = await apiClient.GET("/api/v1/kept-artifacts");
	if (error) throw error;
	return (data?.sets ?? []).map((set) => ({
		sessionId: set.sessionId,
		...(set.projectId ? { projectId: set.projectId } : {}),
		title: set.title || set.sessionId,
		artifactFiles: set.artifactFiles.map(toSessionArtifact),
	}));
}

/**
 * This computer's kept artifacts. A daemon too old to list them lists none;
 * the list is refreshed with the workspace list.
 */
export function useKeptArtifacts() {
	return useQuery({
		queryKey: keptArtifactsQueryKey,
		queryFn: fetchKeptArtifacts,
		enabled: !usesPreviewWorkspaceData && hasTrustedApiBaseUrl(),
		retry: false,
	});
}
