/** Opaque UI cache scope; only the daemon resolves these local references. */
export type LocalAccountRole = "worker" | "orchestrator" | "reviewer";
const prefix = "@local:";
export function localAccountModelScope(projectId: string, role: LocalAccountRole, agentAccountId = ""): string {
	return prefix + new URLSearchParams({ projectId, role, ...(agentAccountId ? { agentAccountId } : {}) }).toString();
}
export function accountModelQuery(scope: string): { projectId?: string; role?: LocalAccountRole; agentAccountId?: string } {
	if (!scope.startsWith(prefix)) return { projectId: scope || undefined };
	const query = new URLSearchParams(scope.slice(prefix.length));
	const role = query.get("role");
	if (role !== "worker" && role !== "orchestrator" && role !== "reviewer") throw new Error("Invalid local account model scope");
	return { projectId: query.get("projectId") || undefined, role, agentAccountId: query.get("agentAccountId") || undefined };
}
