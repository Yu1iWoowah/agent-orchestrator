import { useEffect } from "react";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { LOCAL_HOST } from "../lib/hosts";
import { orchestratorNavigateTarget } from "../lib/navigate-to-session";
import { newestActiveOrchestrator, type WorkspaceSummary } from "../types/workspace";

/** Follow only role-entry navigation, leaving historical session URLs intact. */
export function useFollowProjectOrchestrator(workspaces: WorkspaceSummary[], remoteWorkspaces: WorkspaceSummary[]) {
	const { hostId, projectId, sessionId } = useParams({ strict: false });
	const { followOrchestrator } = useSearch({ strict: false });
	const navigate = useNavigate();
	const workspace = (hostId ? remoteWorkspaces : workspaces).find(
		(value) => value.id === projectId && (value.hostId ?? LOCAL_HOST) === (hostId ?? LOCAL_HOST),
	);
	const successorId = workspace ? newestActiveOrchestrator(workspace.sessions)?.id : undefined;
	useEffect(() => {
		// Keep the current transcript through the gap between kill and spawn.
		if (!followOrchestrator || !projectId || !sessionId || !successorId || successorId === sessionId) return;
		void navigate({ ...orchestratorNavigateTarget(projectId, successorId, hostId), replace: true });
	}, [followOrchestrator, hostId, navigate, projectId, sessionId, successorId]);
}
